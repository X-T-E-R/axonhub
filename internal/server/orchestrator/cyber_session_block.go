package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/anthropic/claudecode"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

const (
	cyberSessionBlockedErrorCode = "session_blocked_by_cyber_policy"
	maxCyberFallbackMessages     = 256
)

var cyberSemanticSessionHeaders = []string{
	"session-id",
	"session_id",
	"conversation_id",
	"x-session-id",
	"x-opencode-session",
	"x-conversation-id",
	"x-claude-code-session-id",
}

var claudeSessionSuffix = regexp.MustCompile(`(?i)_session_([a-f0-9-]+)$`)

type cyberSessionRequestState struct {
	service *biz.SystemService
	ttl     time.Duration

	mu          sync.RWMutex
	lookupKeys  []string
	markKey     string
	observed    bool
	markStarted bool
}

func newCyberSessionRequestState(service *biz.SystemService, ttlSeconds int) *cyberSessionRequestState {
	return &cyberSessionRequestState{
		service: service,
		ttl:     time.Duration(ttlSeconds) * time.Second,
	}
}

func (s *cyberSessionRequestState) setKeys(lookupKeys []string, markKey string) {
	s.mu.Lock()
	s.lookupKeys = append(s.lookupKeys[:0], lookupKeys...)
	s.markKey = markKey
	s.mu.Unlock()
}

func (s *cyberSessionRequestState) keys() ([]string, string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.lookupKeys...), s.markKey, s.observed
}

func (s *cyberSessionRequestState) block(ctx context.Context) (biz.CyberSessionBlockEntry, bool) {
	lookupKeys, _, _ := s.keys()
	return s.service.CyberSessionBlocks(ctx, lookupKeys)
}

func (s *cyberSessionRequestState) blocked(ctx context.Context) bool {
	_, blocked := s.block(ctx)
	return blocked
}

func (s *cyberSessionRequestState) retryBlocked(ctx context.Context) bool {
	_, _, observed := s.keys()
	return observed || s.blocked(ctx)
}

func (s *cyberSessionRequestState) observedCyberPolicy() bool {
	_, _, observed := s.keys()
	return observed
}

func (s *cyberSessionRequestState) markBlocked(ctx context.Context) {
	s.mu.Lock()
	s.observed = true
	markKey := s.markKey
	if s.markStarted || markKey == "" {
		s.mu.Unlock()
		return
	}
	s.markStarted = true
	s.mu.Unlock()

	cacheCtx, cancel := xcontext.DetachWithTimeout(ctx, 500*time.Millisecond)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(cacheCtx, "panic while recording cyber session block", log.Any("panic", recovered))
			}
		}()
		defer cancel()

		if err := s.service.SetCyberSessionBlock(cacheCtx, markKey, s.ttl); err != nil {
			log.Warn(cacheCtx, "failed to record cyber session block", log.Cause(err))
		}
	}()
}

type cyberSessionAdmissionMiddleware struct {
	pipeline.DummyMiddleware

	state    *PersistenceState
	blocking *cyberSessionRequestState
}

func (m *cyberSessionAdmissionMiddleware) Name() string {
	return "cyber-session-admission"
}

func (m *cyberSessionAdmissionMiddleware) OnInboundLlmRequest(ctx context.Context, request *llm.Request) (*llm.Request, error) {
	lookupKeys, markKey, ok := cyberSessionCacheKeys(m.state, request)
	if !ok {
		return request, nil
	}

	m.blocking.setKeys(lookupKeys, markKey)
	entry, blocked := m.blocking.block(ctx)
	if !blocked {
		return request, nil
	}

	statusCode := http.StatusForbidden
	responseBody := []byte(fmt.Sprintf(
		`{"error":{"message":"conversation is locally blocked after an upstream cyber policy refusal until %s","type":"%s","code":"%s"}}`,
		entry.ExpiresAt.UTC().Format(time.RFC3339),
		"permission_error",
		cyberSessionBlockedErrorCode,
	))
	m.state.recordAttemptErrorInfo(&biz.ExecutionErrorInfo{
		StatusCode:   &statusCode,
		ResponseBody: responseBody,
	})

	return nil, &llm.ResponseError{
		StatusCode: statusCode,
		Detail: llm.ErrorDetail{
			Message: "conversation is locally blocked after an upstream cyber policy refusal",
			Type:    "permission_error",
			Code:    cyberSessionBlockedErrorCode,
		},
	}
}

type cyberSessionObservationMiddleware struct {
	pipeline.DummyMiddleware

	blocking *cyberSessionRequestState
}

func (m *cyberSessionObservationMiddleware) Name() string {
	return "cyber-session-observation"
}

func (m *cyberSessionObservationMiddleware) OnOutboundRawError(ctx context.Context, err error) {
	httpErr, ok := errorAsHTTP(err)
	if ok && isCyberPolicyPayload(httpErr.Body) {
		m.blocking.markBlocked(ctx)
	}
}

func (m *cyberSessionObservationMiddleware) OnOutboundRawResponse(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	if response != nil && isCyberPolicyPayload(response.Body) {
		m.blocking.markBlocked(ctx)
	}

	return response, nil
}

func (m *cyberSessionObservationMiddleware) OnOutboundRawStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
	if stream == nil {
		return stream, nil
	}

	return &cyberPolicyObservationStream{
		Stream:   stream,
		ctx:      ctx,
		blocking: m.blocking,
	}, nil
}

type cyberPolicyObservationStream struct {
	streams.Stream[*httpclient.StreamEvent]

	ctx      context.Context
	blocking *cyberSessionRequestState
}

func (s *cyberPolicyObservationStream) Current() *httpclient.StreamEvent {
	event := s.Stream.Current()
	if event != nil && isCyberPolicyPayload(event.Data) {
		s.blocking.markBlocked(s.ctx)
	}

	return event
}

func errorAsHTTP(err error) (*httpclient.Error, bool) {
	var httpErr *httpclient.Error
	ok := errors.As(err, &httpErr)
	return httpErr, ok
}

func isCyberPolicyPayload(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}

	code := gjson.GetBytes(body, "error.code")
	if strings.TrimSpace(code.String()) == "" {
		code = gjson.GetBytes(body, "response.error.code")
	}

	return code.Type == gjson.String && strings.EqualFold(strings.TrimSpace(code.String()), "cyber_policy")
}

func cyberSessionCacheKeys(state *PersistenceState, request *llm.Request) ([]string, string, bool) {
	if state == nil || request == nil || state.APIKey == nil || state.APIKey.ID <= 0 || state.APIKey.ProjectID <= 0 {
		return nil, "", false
	}

	scope := fmt.Sprintf("%d:%d:", state.APIKey.ProjectID, state.APIKey.ID)
	if sessionID := cyberSemanticSessionID(request.RawRequest, request.APIFormat); sessionID != "" {
		key := cyberScopedCacheKey(scope, "session:"+sessionID)
		return []string{key}, key, true
	}

	digests, ok := cyberTranscriptDigests(request.RawRequest)
	if !ok {
		return nil, "", false
	}
	lookupStart := 0
	if len(digests) > maxCyberFallbackMessages {
		lookupStart = len(digests) - maxCyberFallbackMessages
	}

	lookupKeys := make([]string, 0, len(digests)-lookupStart)
	for _, digest := range digests[lookupStart:] {
		lookupKeys = append(lookupKeys, cyberScopedCacheKey(scope, "transcript:"+digest))
	}
	markKey := cyberScopedCacheKey(scope, "transcript:"+digests[len(digests)-1])
	return lookupKeys, markKey, true
}

func cyberScopedCacheKey(scope, conversationID string) string {
	scopeSum := sha256.Sum256([]byte(scope))
	conversationSum := sha256.Sum256([]byte(conversationID))
	// The shared hash tag keeps all transcript-prefix lookups for one API key
	// and project in the same Redis Cluster slot without exposing either ID.
	return "cyber-session-block:{" + hex.EncodeToString(scopeSum[:]) + "}:" + hex.EncodeToString(conversationSum[:])
}

func cyberTranscriptDigests(request *httpclient.Request) ([]string, bool) {
	if request == nil || len(request.Body) == 0 {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false
	}

	history, ok := cyberTranscriptHistory(payload)
	if !ok || len(history) == 0 {
		return nil, false
	}

	h := sha256.New()
	writeCyberTranscriptUnit(h, "axonhub-cyber-session-transcript-v1")
	for _, field := range []string{"instructions", "system"} {
		if value, exists := payload[field]; exists {
			canonical, err := json.Marshal(value)
			if err != nil {
				return nil, false
			}
			writeCyberTranscriptUnit(h, string(canonical))
		}
	}

	digests := make([]string, 0, len(history))
	for _, item := range history {
		canonical, err := json.Marshal(item)
		if err != nil {
			return nil, false
		}
		writeCyberTranscriptUnit(h, string(canonical))
		digests = append(digests, hex.EncodeToString(h.Sum(nil)))
	}

	return digests, true
}

func cyberTranscriptHistory(payload map[string]any) ([]any, bool) {
	if messages, exists := payload["messages"]; exists {
		items, ok := messages.([]any)
		return items, ok
	}

	input, exists := payload["input"]
	if !exists {
		return nil, false
	}
	if items, ok := input.([]any); ok {
		return items, true
	}
	if text, ok := input.(string); ok {
		return []any{text}, true
	}

	return nil, false
}

func writeCyberTranscriptUnit(h hash.Hash, value string) {
	_, _ = fmt.Fprintf(h, "%d:", len(value))
	_, _ = h.Write([]byte(value))
}

func cyberSemanticSessionID(request *httpclient.Request, apiFormats ...llm.APIFormat) string {
	if request == nil {
		return ""
	}

	apiFormat := llm.APIFormat("")
	if len(apiFormats) > 0 {
		apiFormat = apiFormats[0]
	}
	if apiFormat == llm.APIFormatOpenAIResponse || apiFormat == llm.APIFormatOpenAIResponseCompact {
		if sessionID := jsonString(request.Body, "client_metadata.session_id"); sessionID != "" {
			return sessionID
		}
		if sessionID := clientMetadataTurnSessionID(request.Body); sessionID != "" {
			return sessionID
		}
	}

	headers := request.Headers
	if headers == nil {
		headers = make(http.Header)
	}
	if sessionID := codex.ExtractSessionIDFromTurnMetadata(strings.TrimSpace(headers.Get(codex.TurnMetadataHeader))); sessionID != "" {
		return sessionID
	}
	for _, name := range cyberSemanticSessionHeaders {
		if sessionID := strings.TrimSpace(headers.Get(name)); sessionID != "" {
			return sessionID
		}
	}

	if apiFormat == llm.APIFormatAnthropicMessage {
		userID := jsonString(request.Body, "metadata.user_id")
		if parsed := claudecode.ParseUserID(userID); parsed != nil {
			return strings.TrimSpace(parsed.SessionID)
		}
		if match := claudeSessionSuffix.FindStringSubmatch(userID); match != nil {
			return match[1]
		}
	}

	return ""
}

func clientMetadataTurnSessionID(body []byte) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	for _, path := range []string{
		"client_metadata.x-codex-turn-metadata",
		"client_metadata.X-Codex-Turn-Metadata",
	} {
		metadata := gjson.GetBytes(body, path)
		switch metadata.Type {
		case gjson.String:
			if sessionID := codex.ExtractSessionIDFromTurnMetadata(metadata.String()); sessionID != "" {
				return sessionID
			}
		case gjson.JSON:
			sessionID := metadata.Get("session_id")
			if sessionID.Type == gjson.String && strings.TrimSpace(sessionID.String()) != "" {
				return strings.TrimSpace(sessionID.String())
			}
		}
	}
	return ""
}

func jsonString(body []byte, path string) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	value := gjson.GetBytes(body, path)
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}
