package orchestrator

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// Fixed RFC 9562 namespace UUID for AxonHub deterministic UUIDv5 derivation.
var axonIDRewriteUUIDv5Namespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

var (
	prevRespIDNeedle       = []byte(`previous_response_id"`)
	itemRefNeedle          = []byte(`item_reference"`)
	encryptedContentNeedle = []byte(`encrypted_content"`)
)

// captureSessionAffinityIngress snapshots the original immutable ingress request
// headers, body, weak prompt prefix hint, and previous_response_id so retries
// across keys always extract anchors and derive IDs from the original ingress
// values rather than double-hashing a previous attempt's rewritten output.
func captureSessionAffinityIngress(inbound *PersistentInboundTransformer) pipeline.Middleware {
	return pipeline.OnLlmRequest("capture-session-affinity-ingress", func(ctx context.Context, req *llm.Request) (*llm.Request, error) {
		if req == nil {
			return req, nil
		}
		projectID := 0
		if pid, ok := contexts.GetProjectID(ctx); ok {
			projectID = pid
		} else if inbound != nil && inbound.state != nil && inbound.state.APIKey != nil {
			projectID = inbound.state.APIKey.ProjectID
		}

		var rawHeaders http.Header
		var rawBody []byte
		if req.RawRequest != nil {
			if req.RawRequest.Headers != nil {
				rawHeaders = req.RawRequest.Headers.Clone()
			}
			rawBody = req.RawRequest.Body
		}
		prevRespID := ""
		if len(rawBody) > 0 && bytes.Contains(rawBody, prevRespIDNeedle) {
			if res := gjson.GetBytes(rawBody, "previous_response_id"); res.Exists() && res.Type == gjson.String {
				prevRespID = strings.TrimSpace(res.String())
			}
		}

		contexts.WithSessionAffinityRequest(ctx, &contexts.SessionAffinityRequest{
			ProjectID:          projectID,
			RouteKind:          string(req.APIFormat),
			RawHeaders:         rawHeaders,
			RawBody:            rawBody,
			WeakAnchorHint:     req.ChannelKeyAffinityID,
			PreviousResponseID: prevRespID,
		})
		return req, nil
	})
}

// applySessionAffinityAndIDRewrite runs in the outbound wire override phase
// (after protocol outbound transformation and static header/body overrides,
// before request execution auditing and HTTP send).
//
// It enforces:
//  1. Sub2-style strict_replay continuation guard on previous_response_id.
//  2. Zero-reencode HMAC-SHA256 identity derivation/deletion via idRewriteRules.
//  3. Upstream 2xx / first-valid-stream-event confirmation for CPR CAS rebinding
//     and response_id -> credentialKeyID ownership indexing.
func applySessionAffinityAndIDRewrite(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return &sessionAffinityIDRewriteMiddleware{outbound: outbound}
}

type sessionAffinityIDRewriteMiddleware struct {
	pipeline.DummyMiddleware
	outbound *PersistentOutboundTransformer
}

func (m *sessionAffinityIDRewriteMiddleware) Name() string {
	return "session-affinity-id-rewrite"
}

func (m *sessionAffinityIDRewriteMiddleware) OnOutboundRawRequest(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
	if request == nil || m.outbound == nil {
		return request, nil
	}
	ch := m.outbound.GetCurrentChannel()
	if ch == nil {
		return request, nil
	}

	ingressReq, _ := contexts.GetSessionAffinityRequest(ctx)
	decision, _ := contexts.GetSessionAffinityDecision(ctx)

	// 1. Stateful continuation guard (strict_replay / off) for previous_response_id.
	if sa := ch.CompiledSessionAffinity(); sa != nil && sa.Enabled {
		if err := enforceContinuationGuard(request, ingressReq, decision, sa.ContinuationGuard); err != nil {
			contexts.FinishChannelCredentialAttempt(ctx, 0, false, true)
			return nil, err
		}
	}

	// 2. Zero-reencode idRewriteRules execution.
	plan := ch.CompiledIDRewritePlan()
	if plan == nil || plan.Empty() {
		return request, nil
	}

	selectedKeyID := ""
	if decision != nil && decision.SelectedKeyID != "" {
		selectedKeyID = decision.SelectedKeyID
	} else if apiKey, ok := contexts.GetChannelAPIKey(ctx); ok && apiKey != "" {
		selectedKeyID = objects.ChannelAPIKeyFingerprint(apiKey)
	}
	if selectedKeyID == "" {
		return request, nil
	}

	secret, err := m.resolveHMACSecret(ctx)
	if err != nil || len(secret) == 0 {
		// Fail closed when derive rules are configured without a deployment HMAC secret
		// rather than silently forwarding un-rewritten identities. Because this is a
		// local deployment configuration error (not an upstream credential failure),
		// release the attempt cleanly without blaming the API key or FailurePolicy.
		if planHasDeriveRules(plan) {
			contexts.FinishChannelCredentialAttempt(ctx, 0, false, true)
			if err == nil {
				err = biz.ErrIDRewriteSecretMissing()
			}
			return nil, err
		}
	}

	projectID := 0
	var origHeaders http.Header
	var origBody []byte
	if ingressReq != nil {
		projectID = ingressReq.ProjectID
		origHeaders = ingressReq.RawHeaders
		origBody = ingressReq.RawBody
	} else if pid, ok := contexts.GetProjectID(ctx); ok {
		projectID = pid
	}
	anchorValue := ""
	switched := false
	if decision != nil {
		anchorValue = decision.AnchorValue
		switched = decision.Switched
	}

	ApplyCompiledIDRewritePlan(
		plan,
		secret,
		strconv.Itoa(projectID),
		ch.CredentialOwnerID(),
		selectedKeyID,
		anchorValue,
		switched,
		origHeaders,
		origBody,
		request,
	)

	return request, nil
}

func (m *sessionAffinityIDRewriteMiddleware) OnOutboundRawResponse(ctx context.Context, response *httpclient.Response) (*httpclient.Response, error) {
	if response != nil && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		respID := extractUpstreamResponseID(response.Body)
		contexts.ConfirmSessionAffinitySuccess(ctx, respID)
	}
	return response, nil
}

func (m *sessionAffinityIDRewriteMiddleware) OnOutboundRawStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
	if stream == nil {
		return nil, nil
	}
	return &sessionAffinityConfirmStream{
		ctx:    ctx,
		stream: stream,
	}, nil
}

func (m *sessionAffinityIDRewriteMiddleware) OnDirectPassThroughStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
	if stream == nil {
		return nil, nil
	}
	return &sessionAffinityConfirmStream{
		ctx:    ctx,
		stream: stream,
	}, nil
}

func (m *sessionAffinityIDRewriteMiddleware) resolveHMACSecret(ctx context.Context) ([]byte, error) {
	if m.outbound != nil && m.outbound.state != nil && m.outbound.state.ChannelService != nil {
		return m.outbound.state.ChannelService.IDRewriteSecret(ctx)
	}
	var sys *biz.SystemService
	if m.outbound != nil && m.outbound.state != nil {
		sys = m.outbound.state.SystemService
	}
	return biz.ResolveIDRewriteSecret(ctx, sys)
}

type sessionAffinityConfirmStream struct {
	ctx           context.Context
	stream        streams.Stream[*httpclient.StreamEvent]
	confirmed     bool
	recordedResp  bool
}

func (s *sessionAffinityConfirmStream) Next() bool {
	return s.stream.Next()
}

func (s *sessionAffinityConfirmStream) Current() *httpclient.StreamEvent {
	ev := s.stream.Current()
	if isStreamSuccessEvent(ev) {
		respID := extractUpstreamResponseID(ev.Data)
		if !s.confirmed {
			s.confirmed = true
			if respID != "" {
				s.recordedResp = true
			}
			contexts.ConfirmSessionAffinitySuccess(s.ctx, respID)
		} else if !s.recordedResp && respID != "" {
			s.recordedResp = true
			if dec, ok := contexts.GetSessionAffinityDecision(s.ctx); ok && dec != nil {
				dec.RecordResponseOwner(respID)
			}
		}
	}
	return ev
}

func isStreamSuccessEvent(ev *httpclient.StreamEvent) bool {
	if ev == nil {
		return false
	}
	data := bytes.TrimSpace(ev.Data)
	// Reject empty frames, bare [DONE], SSE comments/keepalives, and incomplete non-JSON frames.
	if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' || bytes.Equal(data, []byte("[DONE]")) {
		return false
	}
	if isStreamErrorEvent(ev) {
		return false
	}

	evType := strings.ToLower(strings.TrimSpace(ev.Type))
	if evType == "ping" || evType == "keepalive" {
		return false
	}

	if bytes.Contains(data, []byte(`"type"`)) {
		jsonType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(data, "type").String()))
		if jsonType != "" {
			return isPositiveStreamEventType(jsonType)
		}
	}
	if evType != "" && isPositiveStreamEventType(evType) {
		return true
	}

	// OpenAI Chat Completions / Legacy Completions chunk with at least one choice.
	if bytes.Contains(data, []byte(`"choices"`)) && gjson.GetBytes(data, "choices.0").Exists() {
		return true
	}
	// Gemini generateContent stream chunk with at least one candidate.
	if bytes.Contains(data, []byte(`"candidates"`)) && gjson.GetBytes(data, "candidates.0").Exists() {
		return true
	}
	return false
}

func isPositiveStreamEventType(t string) bool {
	switch t {
	// Anthropic Messages SSE business events (explicitly excludes "ping").
	case "message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop":
		return true
	// OpenAI Responses API SSE business events (explicitly excludes failed/incomplete/error).
	case "response.created",
		"response.in_progress",
		"response.completed",
		"response.output_item.added",
		"response.output_item.done",
		"response.content_part.added",
		"response.content_part.done",
		"response.output_text.delta",
		"response.output_text.done",
		"response.refusal.delta",
		"response.refusal.done",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.reasoning_text.delta",
		"response.reasoning_text.done",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done":
		return true
	default:
		return false
	}
}

func isStreamErrorEvent(ev *httpclient.StreamEvent) bool {
	if ev == nil {
		return true
	}
	evType := strings.ToLower(strings.TrimSpace(ev.Type))
	if evType == "error" || evType == "response.failed" || evType == "response.incomplete" {
		return true
	}
	data := bytes.TrimSpace(ev.Data)
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return false
	}
	if bytes.Contains(data, []byte(`"type"`)) {
		if t := strings.ToLower(gjson.GetBytes(data, "type").String()); t == "error" || t == "response.failed" || t == "response.incomplete" {
			return true
		}
	}
	if bytes.Contains(data, []byte(`"error"`)) {
		if errField := gjson.GetBytes(data, "error"); errField.Exists() && errField.Type != gjson.Null && errField.Type != gjson.False && strings.TrimSpace(errField.Raw) != "" {
			return true
		}
	}
	return false
}

func planHasDeriveRules(plan *biz.CompiledIDRewritePlan) bool {
	if plan == nil {
		return false
	}
	for i := range plan.HeaderRules {
		if plan.HeaderRules[i].Op == objects.ChannelIDRewriteOpDerive {
			return true
		}
	}
	for i := range plan.JSONRules {
		if plan.JSONRules[i].Op == objects.ChannelIDRewriteOpDerive {
			return true
		}
	}
	return false
}

func (s *sessionAffinityConfirmStream) Err() error {
	return s.stream.Err()
}

func (s *sessionAffinityConfirmStream) Close() error {
	return s.stream.Close()
}

func extractUpstreamResponseID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if !bytes.Contains(body, []byte(`"id"`)) {
		return ""
	}
	if res := gjson.GetBytes(body, "response.id"); res.Exists() && res.Type == gjson.String {
		if id := strings.TrimSpace(res.String()); id != "" {
			return id
		}
	}
	if res := gjson.GetBytes(body, "id"); res.Exists() && res.Type == gjson.String {
		if id := strings.TrimSpace(res.String()); strings.HasPrefix(id, "resp_") {
			return id
		}
	}
	return ""
}

// enforceContinuationGuard implements stateful continuation gating for
// previous_response_id:
//   - strict_replay (default):
//     * Same owner AND final outbound previous_response_id matches ingress cursor
//       verbatim (not deleted or mutated by a static body override): allow untouched.
//     * Any other case (different/unknown owner, or cursor altered/removed before
//       dispatch): fail closed with 409 Conflict (continuation_owner_unavailable)
//       so the client can retry cleanly with full cursorless history—never silently
//       strip previous_response_id and pretend a truncated window is complete.
//   - off:
//     * Same owner with unchanged cursor: allow untouched.
//     * Cross-key / unknown owner: strip previous_response_id and server-state
//       references (item_reference items and encrypted_content fields) so the
//       request can proceed best-effort on the alternate credential.
func enforceContinuationGuard(
	request *httpclient.Request,
	ingress *contexts.SessionAffinityRequest,
	decision *contexts.SessionAffinityDecision,
	guard objects.ChannelContinuationGuard,
) error {
	if request == nil || len(request.Body) == 0 {
		return nil
	}
	outPrev := ""
	if bytes.Contains(request.Body, prevRespIDNeedle) {
		if res := gjson.GetBytes(request.Body, "previous_response_id"); res.Exists() && res.Type == gjson.String {
			outPrev = strings.TrimSpace(res.String())
		}
	}
	inPrev := ""
	if ingress != nil {
		inPrev = strings.TrimSpace(ingress.PreviousResponseID)
	}
	if outPrev == "" && inPrev == "" {
		return nil
	}

	ownership := contexts.SessionStateOwnershipUnknown
	if decision != nil && decision.StateOwnership != "" {
		ownership = decision.StateOwnership
	}

	if guard == objects.ChannelContinuationGuardOff {
		if ownership == contexts.SessionStateOwnershipSame && (inPrev == "" || outPrev == inPrev) {
			return nil
		}
		cleaned, err := stripContinuationStateForCrossKey(request.Body)
		if err != nil {
			return err
		}
		request.Body = cleaned
		return nil
	}

	// strict_replay: require Same owner AND exact preservation of the ingress cursor.
	if ownership == contexts.SessionStateOwnershipSame && inPrev != "" && outPrev == inPrev {
		return nil
	}

	errPayload, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "conflict",
			"code":    "continuation_owner_unavailable",
			"message": "The credential that owns previous_response_id is unavailable; resend with full replayable conversation input without previous_response_id.",
		},
	})
	return &httpclient.Error{
		StatusCode: http.StatusConflict,
		Body:       errPayload,
	}
}

func isContinuationGuardConflictError(err error) bool {
	var httpErr *httpclient.Error
	return errors.As(err, &httpErr) &&
		httpErr.StatusCode == http.StatusConflict &&
		bytes.Contains(httpErr.Body, []byte("continuation_owner_unavailable"))
}

func stripContinuationStateForCrossKey(body []byte) ([]byte, error) {
	updated := body
	if bytes.Contains(updated, prevRespIDNeedle) {
		var err error
		updated, err = sjson.DeleteBytes(updated, "previous_response_id")
		if err != nil {
			return nil, err
		}
	}
	if !bytes.Contains(updated, itemRefNeedle) && !bytes.Contains(updated, encryptedContentNeedle) {
		return updated, nil
	}

	input := gjson.GetBytes(updated, "input")
	if !input.Exists() || !input.IsArray() {
		return updated, nil
	}

	var deleteItemIdxs []int
	var clearEncryptedIdxs []int
	idx := 0
	input.ForEach(func(_, item gjson.Result) bool {
		if strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "item_reference") {
			deleteItemIdxs = append(deleteItemIdxs, idx)
		} else if item.Get("encrypted_content").Exists() {
			clearEncryptedIdxs = append(clearEncryptedIdxs, idx)
		}
		idx++
		return true
	})

	// Clear encrypted_content first while indices are unchanged.
	for _, i := range clearEncryptedIdxs {
		var err error
		updated, err = sjson.DeleteBytes(updated, "input."+strconv.Itoa(i)+".encrypted_content")
		if err != nil {
			return nil, err
		}
	}
	// Delete item_reference entries in descending index order so earlier indices stay valid.
	for j := len(deleteItemIdxs) - 1; j >= 0; j-- {
		var err error
		updated, err = sjson.DeleteBytes(updated, "input."+strconv.Itoa(deleteItemIdxs[j]))
		if err != nil {
			return nil, err
		}
	}
	return updated, nil
}

// ApplyCompiledIDRewritePlan executes precompiled header and JSON identity
// rewrite rules against request in place without full JSON Unmarshal/Marshal.
//
// Zero-allocation guarantee:
//   - When plan has no JSON rules, request.Body is never inspected or copied.
//   - When a JSON rule's target path is absent and ifMissing == "skip", request.Body
//     is untouched with 0 allocations.
//   - When an existing JSON string field is derived, it is patched in a single
//     byte splice using gjson's byte offset (no second scan over the payload).
func ApplyCompiledIDRewritePlan(
	plan *biz.CompiledIDRewritePlan,
	secret []byte,
	tenantID string,
	ownerChannelID int,
	selectedKeyID string,
	anchorValue string,
	switched bool,
	origHeaders http.Header,
	origBody []byte,
	request *httpclient.Request,
) {
	if plan == nil || plan.Empty() || request == nil {
		return
	}

	// 1. Apply header rules.
	if len(plan.HeaderRules) > 0 {
		if request.Headers == nil {
			request.Headers = make(http.Header)
		}
		for i := range plan.HeaderRules {
			rule := &plan.HeaderRules[i]
			if rule.When == objects.ChannelIDRewriteWhenOnKeySwitch && !switched {
				continue
			}
			origVal := ""
			if origHeaders != nil {
				origVal = origHeaders.Get(rule.HeaderKey)
			}
			if origVal == "" {
				origVal = request.Headers.Get(rule.HeaderKey)
			}
			if rule.Op == objects.ChannelIDRewriteOpDelete {
				request.Headers.Del(rule.HeaderKey)
				continue
			}
			if origVal == "" {
				if rule.IfMissing != objects.ChannelIDRewriteIfMissingGenerate {
					continue
				}
				origVal = anchorValue
				if origVal == "" {
					origVal = "default"
				}
			}
			derived := DeriveIdentityValue(secret, tenantID, ownerChannelID, selectedKeyID, rule, origVal)
			request.Headers.Set(rule.HeaderKey, derived)
		}
	}

	// 2. Apply JSON body rules (zero work on body when HasJSONRules is false).
	if !plan.HasJSONRules || len(request.Body) == 0 {
		return
	}
	body := request.Body
	origSameAsBody := len(origBody) == 0 || (&origBody[0] == &body[0] && len(origBody) == len(body))

	for i := range plan.JSONRules {
		rule := &plan.JSONRules[i]
		if rule.When == objects.ChannelIDRewriteWhenOnKeySwitch && !switched {
			continue
		}
		var res gjson.Result
		if len(rule.LeafKeyNeedle) == 0 || bytes.Contains(body, rule.LeafKeyNeedle) {
			res = gjson.GetBytes(body, rule.EscapedPath)
		}
		if !res.Exists() {
			if rule.Op == objects.ChannelIDRewriteOpDelete || rule.IfMissing != objects.ChannelIDRewriteIfMissingGenerate {
				continue
			}
			seedVal := anchorValue
			if seedVal == "" {
				seedVal = "default"
			}
			derived := DeriveIdentityValue(secret, tenantID, ownerChannelID, selectedKeyID, rule, seedVal)
			if updated, err := sjson.SetBytesOptions(body, rule.EscapedPath, derived, &sjson.Options{Optimistic: true}); err == nil {
				body = updated
				origSameAsBody = false
			}
			continue
		}

		if rule.Op == objects.ChannelIDRewriteOpDelete {
			if updated, err := sjson.DeleteBytes(body, rule.EscapedPath); err == nil {
				body = updated
				origSameAsBody = false
			}
			continue
		}

		if res.Type != gjson.String {
			continue
		}

		origVal := res.String()
		if !origSameAsBody && (len(rule.LeafKeyNeedle) == 0 || bytes.Contains(origBody, rule.LeafKeyNeedle)) {
			if origRes := gjson.GetBytes(origBody, rule.EscapedPath); origRes.Exists() && origRes.Type == gjson.String {
				origVal = origRes.String()
			}
		}
		derived := DeriveIdentityValue(secret, tenantID, ownerChannelID, selectedKeyID, rule, origVal)
		if derived == res.String() {
			continue
		}

		// Fast path: splice directly at res.Index without a second JSON scan.
		// When request.Body is distinct from origBody and the quoted value has the
		// exact same byte length (true for fixed-width formats like uuid_v8, hex32,
		// hex64, and anthropic_user_id), overwrite in place with zero body allocation.
		if res.Index > 0 && res.Index+len(res.Raw) <= len(body) && body[res.Index] == '"' {
			body = spliceJSONStringAt(body, res.Index, len(res.Raw), derived, !origSameAsBody)
			origSameAsBody = false
			continue
		}
		if updated, err := sjson.SetBytesOptions(body, rule.EscapedPath, derived, &sjson.Options{Optimistic: true}); err == nil {
			body = updated
			origSameAsBody = false
		}
	}
	request.Body = body
}

func spliceJSONStringAt(body []byte, start int, oldRawLen int, newValue string, allowInPlace bool) []byte {
	var quotedBuf [192]byte
	quoted := appendJSONQuotedString(quotedBuf[:0], newValue)
	end := start + oldRawLen
	if allowInPlace {
		if len(quoted) <= oldRawLen {
			// Same-width or shrinking replacement: overwrite in place and pad trailing
			// bytes with RFC 8259 JSON whitespace (' ') — 0 bytes allocated!
			copy(body[start:], quoted)
			for i := start + len(quoted); i < end; i++ {
				body[i] = ' '
			}
			return body
		}
		newLen := len(body) - oldRawLen + len(quoted)
		if cap(body) >= newLen {
			oldLen := len(body)
			grown := body[:newLen]
			copy(grown[start+len(quoted):], body[end:oldLen])
			copy(grown[start:], quoted)
			return grown
		}
	}
	out := make([]byte, start+len(quoted)+len(body)-end)
	copy(out, body[:start])
	copy(out[start:], quoted)
	copy(out[start+len(quoted):], body[end:])
	return out
}

func appendJSONQuotedString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				const hexDigits = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0x0f])
			} else {
				dst = append(dst, c)
			}
		}
	}
	dst = append(dst, '"')
	return dst
}

// DeriveIdentityValue computes a deterministic, unambiguous HMAC-SHA256 derived
// identifier in the requested RFC-compliant format.
func DeriveIdentityValue(
	secret []byte,
	tenantID string,
	ownerChannelID int,
	selectedKeyID string,
	rule *biz.CompiledIDRewriteRule,
	originalValue string,
) string {
	if rule.Format == objects.ChannelIDRewriteFormatAnthropicUserID {
		return deriveAnthropicUserID(secret, tenantID, ownerChannelID, selectedKeyID, rule.Target, originalValue)
	}

	var macBuf [sha256.Size]byte
	computeIdentityMAC(macBuf[:0], secret, tenantID, ownerChannelID, selectedKeyID, rule.Target, "", originalValue)

	switch rule.Format {
	case objects.ChannelIDRewriteFormatHex32:
		var out [32]byte
		hex.Encode(out[:], macBuf[:16])
		return string(out[:])
	case objects.ChannelIDRewriteFormatHex64:
		var out [64]byte
		hex.Encode(out[:], macBuf[:32])
		return string(out[:])
	case objects.ChannelIDRewriteFormatPrefixHex8:
		var out [8]byte
		hex.Encode(out[:], macBuf[:4])
		return rule.Prefix + string(out[:])
	case objects.ChannelIDRewriteFormatPrefixHex16:
		var out [16]byte
		hex.Encode(out[:], macBuf[:8])
		return rule.Prefix + string(out[:])
	case objects.ChannelIDRewriteFormatUUIDv8:
		return formatUUIDv8(macBuf[:16])
	case objects.ChannelIDRewriteFormatUUIDv5:
		// Strictly RFC 9562 Section 5.5 SHA-1 UUIDv5 (never SHA-256 masquerading as v5).
		return uuid.NewSHA1(axonIDRewriteUUIDv5Namespace, macBuf[:]).String()
	default:
		var out [32]byte
		hex.Encode(out[:], macBuf[:16])
		return string(out[:])
	}
}

func computeIdentityMAC(
	dst []byte,
	secret []byte,
	tenantID string,
	ownerChannelID int,
	selectedKeyID string,
	target string,
	subField string,
	originalValue string,
) []byte {
	var msgBuf [256]byte
	msg := append(msgBuf[:0], "axon-id-v1"...)
	msg = append(msg, 0)
	msg = append(msg, tenantID...)
	msg = append(msg, 0)
	msg = strconv.AppendInt(msg, int64(ownerChannelID), 10)
	msg = append(msg, 0)
	msg = append(msg, selectedKeyID...)
	msg = append(msg, 0)
	msg = append(msg, target...)
	if subField != "" {
		msg = append(msg, 0)
		msg = append(msg, subField...)
	}
	msg = append(msg, 0)
	msg = append(msg, originalValue...)

	h := hmac.New(sha256.New, secret)
	_, _ = h.Write(msg)
	return h.Sum(dst)
}

// formatUUIDv8 encodes 16 bytes into a standards-compliant RFC 9562 UUIDv8
// (version nibble 0x8, RFC 4122/9562 variant bits 10xx).
func formatUUIDv8(src []byte) string {
	var b [16]byte
	copy(b[:], src[:16])
	b[6] = (b[6] & 0x0f) | 0x80 // version 8
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 9562 variant
	return uuid.UUID(b).String()
}

func appendUUIDv8(dst []byte, src []byte) []byte {
	const hexDigits = "0123456789abcdef"
	var b [16]byte
	copy(b[:], src[:16])
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			dst = append(dst, '-')
		}
		dst = append(dst, hexDigits[b[i]>>4], hexDigits[b[i]&0x0f])
	}
	return dst
}

// deriveAnthropicUserID handles Claude / Kimi composite metadata.user_id values:
//   user_<64hex>_account_<uuid_or_empty>_session_<uuid>
// Deterministically derives user_ and _session_ (and _account_ when non-empty)
// scoped to selectedKeyID; falls back to user_<64hex> for non-composite inputs.
func deriveAnthropicUserID(
	secret []byte,
	tenantID string,
	ownerChannelID int,
	selectedKeyID string,
	target string,
	originalValue string,
) string {
	const (
		userPrefix = "user_"
		acctSep    = "_account_"
		sessSep    = "_session_"
	)
	if strings.HasPrefix(originalValue, userPrefix) {
		rest := originalValue[len(userPrefix):]
		acctIdx := strings.Index(rest, acctSep)
		if acctIdx > 0 {
			userPart := rest[:acctIdx]
			afterAcct := rest[acctIdx+len(acctSep):]
			sessIdx := strings.Index(afterAcct, sessSep)
			if sessIdx >= 0 {
				acctPart := afterAcct[:sessIdx]
				sessPart := afterAcct[sessIdx+len(sessSep):]

				var userMAC, sessMAC [sha256.Size]byte
				computeIdentityMAC(userMAC[:0], secret, tenantID, ownerChannelID, selectedKeyID, target, "user", userPart)
				computeIdentityMAC(sessMAC[:0], secret, tenantID, ownerChannelID, selectedKeyID, target, "session", sessPart)

				var outBuf [160]byte
				out := append(outBuf[:0], userPrefix...)
				var hexBuf [64]byte
				hex.Encode(hexBuf[:], userMAC[:32])
				out = append(out, hexBuf[:]...)
				out = append(out, acctSep...)
				if acctPart != "" {
					var acctMAC [sha256.Size]byte
					computeIdentityMAC(acctMAC[:0], secret, tenantID, ownerChannelID, selectedKeyID, target, "account", acctPart)
					out = appendUUIDv8(out, acctMAC[:16])
				}
				out = append(out, sessSep...)
				out = appendUUIDv8(out, sessMAC[:16])
				return string(out)
			}
		}
	}
	var fullMAC [sha256.Size]byte
	computeIdentityMAC(fullMAC[:0], secret, tenantID, ownerChannelID, selectedKeyID, target, "", originalValue)
	var outBuf [69]byte
	copy(outBuf[:5], userPrefix)
	hex.Encode(outBuf[5:], fullMAC[:32])
	return string(outBuf[:])
}
