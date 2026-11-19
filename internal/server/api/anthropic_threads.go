package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

const (
	anthropicMessageThreadsBeta         = "message-threads-2026-08-12"
	anthropicCompactBeta                = "compact-2026-09-04"
	anthropicMaxCacheControlBreakpoints = 4
)

type anthropicThreadError struct {
	status      int
	errType     string
	message     string
	errorCode   string
	unsupported []string
}

func newAnthropicThreadInvalidRequestError(message string) *anthropicThreadError {
	return &anthropicThreadError{
		status:  http.StatusBadRequest,
		errType: "invalid_request_error",
		message: message,
	}
}

func newAnthropicThreadNotFoundError() *anthropicThreadError {
	return &anthropicThreadError{
		status:    http.StatusNotFound,
		errType:   "not_found_error",
		message:   "Thread not found or expired.",
		errorCode: "thread_not_found",
	}
}

func newAnthropicThreadContinueInFlightError() *anthropicThreadError {
	return &anthropicThreadError{
		status:    http.StatusBadRequest,
		errType:   "invalid_request_error",
		message:   "Another continue request from this response id is still running.",
		errorCode: "thread_continue_in_flight",
	}
}

func newAnthropicThreadAlreadyContinuedError() *anthropicThreadError {
	return &anthropicThreadError{
		status:    http.StatusBadRequest,
		errType:   "invalid_request_error",
		message:   "This response id has already been continued.",
		errorCode: "thread_already_continued",
	}
}

func newAnthropicThreadFingerprintMismatchError(diffs []string) *anthropicThreadError {
	return &anthropicThreadError{
		status:    http.StatusBadRequest,
		errType:   "invalid_request_error",
		message:   "Fixed request fields do not match the thread: " + strings.Join(diffs, ", ") + ".",
		errorCode: "thread_fingerprint_mismatch",
	}
}

func newAnthropicThreadUnsupportedRequestError(unsupported []string) *anthropicThreadError {
	return &anthropicThreadError{
		status:      http.StatusBadRequest,
		errType:     "invalid_request_error",
		message:     "Request uses feature(s) unsupported with message threads: " + strings.Join(unsupported, ", ") + ".",
		errorCode:   "thread_unsupported_request",
		unsupported: unsupported,
	}
}

func writeAnthropicThreadError(c *gin.Context, threadErr *anthropicThreadError) {
	if threadErr == nil {
		return
	}
	detail := map[string]any{
		"type":    threadErr.errType,
		"message": threadErr.message,
	}
	if threadErr.errorCode != "" {
		details := map[string]any{
			"error_code": threadErr.errorCode,
		}
		if len(threadErr.unsupported) > 0 {
			details["unsupported"] = threadErr.unsupported
		}
		detail["details"] = details
	}
	payload := map[string]any{
		"type":  "error",
		"error": detail,
	}
	if requestID, ok := contexts.GetRequestID(c.Request.Context()); ok && requestID != "" {
		payload["request_id"] = requestID
	}
	c.JSON(threadErr.status, payload)
}

func parseAnthropicBetaTokens(headers http.Header) []string {
	if headers == nil {
		return nil
	}
	var tokens []string
	for _, value := range headers.Values("Anthropic-Beta") {
		for token := range strings.SplitSeq(value, ",") {
			if trimmed := strings.TrimSpace(token); trimmed != "" {
				tokens = append(tokens, trimmed)
			}
		}
	}
	return tokens
}

func hasAnthropicBetaToken(tokens []string, target string) bool {
	return slices.Contains(tokens, target)
}

func stripAnthropicBetaToken(headers http.Header, target string) http.Header {
	cloned := headers.Clone()
	if cloned == nil {
		cloned = make(http.Header)
	}
	tokens := parseAnthropicBetaTokens(cloned)
	kept := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if token == target {
			continue
		}
		kept = append(kept, token)
	}
	cloned.Del("Anthropic-Beta")
	if len(kept) > 0 {
		cloned.Set("Anthropic-Beta", strings.Join(kept, ","))
	}
	return cloned
}

func anthropicThreadScopeFromContext(ctx context.Context) (anthropicThreadScope, bool) {
	key, ok := contexts.GetAPIKey(ctx)
	if !ok || key == nil || key.ID <= 0 {
		return anthropicThreadScope{}, false
	}
	projectID, ok := contexts.GetProjectID(ctx)
	if !ok || projectID <= 0 {
		return anthropicThreadScope{}, false
	}
	return anthropicThreadScope{projectID: projectID, apiKeyID: key.ID}, true
}

func isJSONNullOrEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// canonicalJSONValue returns a deterministic JSON representation with sorted
// object keys (adapted from anthropic-sdk-go's betaThreadJSON, using UseNumber
// to preserve integer precision).
func canonicalJSONValue(raw json.RawMessage) string {
	if isJSONNullOrEmpty(raw) {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	data, err := json.Marshal(decoded)
	if err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return string(data)
}

// canonicalMCPServers canonicalizes mcp_servers while ignoring each server
// object's authorization_token per the message-threads-2026-08-12 spec.
func canonicalMCPServers(raw json.RawMessage) string {
	if isJSONNullOrEmpty(raw) {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	if servers, ok := decoded.([]any); ok {
		for _, item := range servers {
			if obj, ok := item.(map[string]any); ok {
				delete(obj, "authorization_token")
			}
		}
	}
	data, err := json.Marshal(decoded)
	if err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return string(data)
}

func canonicalContainerSkills(raw json.RawMessage) string {
	if isJSONNullOrEmpty(raw) {
		return ""
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ""
	}
	var container map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &container); err != nil {
		return ""
	}
	skills, ok := container["skills"]
	if !ok {
		return ""
	}
	return canonicalJSONValue(skills)
}

func canonicalBetaSet(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	sorted := slices.Clone(tokens)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	return strings.Join(sorted, ",")
}

func computeAnthropicThreadFingerprint(topFields map[string]json.RawMessage, betaTokens []string) anthropicThreadFingerprint {
	return anthropicThreadFingerprint{
		model:             canonicalJSONValue(topFields["model"]),
		toolChoice:        canonicalJSONValue(topFields["tool_choice"]),
		thinking:          canonicalJSONValue(topFields["thinking"]),
		mcpServers:        canonicalMCPServers(topFields["mcp_servers"]),
		contextManagement: canonicalJSONValue(topFields["context_management"]),
		outputConfig:      canonicalJSONValue(topFields["output_config"]),
		containerSkills:   canonicalContainerSkills(topFields["container"]),
		cacheControl:      canonicalJSONValue(topFields["cache_control"]),
		anthropicBeta:     canonicalBetaSet(betaTokens),
	}
}

func diffAnthropicThreadFingerprint(expected, actual anthropicThreadFingerprint) []string {
	var diffs []string
	if expected.model != actual.model {
		diffs = append(diffs, "model")
	}
	if expected.toolChoice != actual.toolChoice {
		diffs = append(diffs, "tool_choice")
	}
	if expected.thinking != actual.thinking {
		diffs = append(diffs, "thinking")
	}
	if expected.mcpServers != actual.mcpServers {
		diffs = append(diffs, "mcp_servers")
	}
	if expected.contextManagement != actual.contextManagement {
		diffs = append(diffs, "context_management")
	}
	if expected.outputConfig != actual.outputConfig {
		diffs = append(diffs, "output_config")
	}
	if expected.containerSkills != actual.containerSkills {
		diffs = append(diffs, "container.skills")
	}
	if expected.cacheControl != actual.cacheControl {
		diffs = append(diffs, "cache_control")
	}
	if expected.anthropicBeta != actual.anthropicBeta {
		diffs = append(diffs, "anthropic-beta")
	}
	return diffs
}

// hasUnsupportedCompaction reports whether the request combines threads with
// compaction (top-level compaction param, compact-2026-09-04 beta header, or
// a compaction edit inside context_management.edits).
func hasUnsupportedCompaction(topFields map[string]json.RawMessage, betaTokens []string) bool {
	if hasAnthropicBetaToken(betaTokens, anthropicCompactBeta) {
		return true
	}
	if compaction, ok := topFields["compaction"]; ok && !isJSONNullOrEmpty(compaction) {
		return true
	}
	if cm, ok := topFields["context_management"]; ok && !isJSONNullOrEmpty(cm) {
		edits := gjson.GetBytes(cm, "edits")
		if edits.IsArray() {
			for _, edit := range edits.Array() {
				editType := strings.ToLower(edit.Get("type").String())
				if strings.HasPrefix(editType, "compact") {
					return true
				}
			}
		}
	}
	return false
}

// countBlockCacheControl counts cache_control breakpoints within a content
// block, system block, tool definition, or message object. Following
// anthropics/anthropic-sdk-go's withoutBlockCacheControl traversal, it enters
// only the keys that hold blocks or tool definitions ("content", "source",
// "tool", "definition", "tool_references") so a "cache_control" key inside
// free-form fields such as tool_use.input or input_schema is never counted.
func countBlockCacheControl(v gjson.Result) int {
	if v.IsArray() {
		total := 0
		for _, item := range v.Array() {
			total += countBlockCacheControl(item)
		}
		return total
	}
	if !v.IsObject() {
		return 0
	}
	total := 0
	v.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "cache_control":
			if value.Type != gjson.Null {
				total++
			}
		case "content", "source", "tool", "definition", "tool_references":
			total += countBlockCacheControl(value)
		}
		return true
	})
	return total
}

// withoutBlockCacheControl returns the JSON of block without its cache_control
// and that of the blocks and tool definitions under it. Adapted directly from
// anthropics/anthropic-sdk-go/betamessagethread.go.
func withoutBlockCacheControl(block gjson.Result) string {
	if block.IsArray() {
		items := block.Array()
		parts := make([]string, 0, len(items))
		for _, nested := range items {
			parts = append(parts, withoutBlockCacheControl(nested))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	if !block.IsObject() {
		return block.Raw
	}
	var members []string
	block.ForEach(func(key, value gjson.Result) bool {
		raw := value.Raw
		switch key.Str {
		case "cache_control":
			return true
		case "content", "source", "tool", "definition", "tool_references":
			raw = withoutBlockCacheControl(value)
		}
		members = append(members, key.Raw+":"+raw)
		return true
	})
	return "{" + strings.Join(members, ",") + "}"
}

func countRawCacheControl(raw json.RawMessage) int {
	if isJSONNullOrEmpty(raw) || !bytes.Contains(raw, []byte(`"cache_control"`)) {
		return 0
	}
	return countBlockCacheControl(gjson.ParseBytes(raw))
}

func stripMessageCacheControl(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || !bytes.Contains(raw, []byte(`"cache_control"`)) {
		return raw
	}
	stripped := withoutBlockCacheControl(gjson.ParseBytes(raw))
	return json.RawMessage(stripped)
}

func parseMessagesArray(raw json.RawMessage) ([]json.RawMessage, *anthropicThreadError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, newAnthropicThreadInvalidRequestError("messages is required and must be a JSON array.")
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil, newAnthropicThreadInvalidRequestError("messages must be a JSON array of message objects.")
	}
	for i, msg := range messages {
		trimmed := bytes.TrimSpace(msg)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, newAnthropicThreadInvalidRequestError(fmt.Sprintf("messages[%d] must be a JSON object.", i))
		}
		messages[i] = append(json.RawMessage(nil), trimmed...)
	}
	return messages, nil
}

func buildStoredMessages(rawMessages []json.RawMessage) ([]anthropicStoredMessage, int) {
	stored := make([]anthropicStoredMessage, len(rawMessages))
	totalBreakpoints := 0
	for i, raw := range rawMessages {
		bp := countRawCacheControl(raw)
		stored[i] = anthropicStoredMessage{
			raw:           raw,
			cacheControls: bp,
		}
		totalBreakpoints += bp
	}
	return stored, totalBreakpoints
}

// materializeThreadMessages joins stored historical messages with the new turn's
// messages while enforcing Anthropic's 4-breakpoint cache_control ceiling:
// system, tools, top-level cache_control, and the newest turn's messages keep
// their breakpoints verbatim; historical message breakpoints are preserved from
// newest to oldest only while remaining within the 4-breakpoint budget and
// stripped from older turns.
func materializeThreadMessages(
	stored []*anthropicStoredMessage,
	newMessages []json.RawMessage,
	fixedBreakpoints int,
	newBreakpoints int,
) json.RawMessage {
	budget := max(0, anthropicMaxCacheControlBreakpoints-fixedBreakpoints-newBreakpoints)

	resolvedStored := make([]json.RawMessage, len(stored))
	for i := len(stored) - 1; i >= 0; i-- {
		msg := stored[i]
		switch {
		case msg.cacheControls == 0:
			resolvedStored[i] = msg.raw
		case msg.cacheControls <= budget:
			resolvedStored[i] = msg.raw
			budget -= msg.cacheControls
		default:
			resolvedStored[i] = stripMessageCacheControl(msg.raw)
		}
	}

	var buf bytes.Buffer
	buf.WriteByte('[')
	wrote := false
	for _, raw := range resolvedStored {
		if wrote {
			buf.WriteByte(',')
		}
		buf.Write(raw)
		wrote = true
	}
	for _, raw := range newMessages {
		if wrote {
			buf.WriteByte(',')
		}
		buf.Write(raw)
		wrote = true
	}
	buf.WriteByte(']')
	return json.RawMessage(buf.Bytes())
}

type anthropicThreadParam struct {
	Type              string `json:"type"`
	PreviousMessageID string `json:"previous_message_id"`
}

func parseAnthropicThreadParam(raw json.RawMessage) (anthropicThreadParam, *anthropicThreadError) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return anthropicThreadParam{}, newAnthropicThreadInvalidRequestError("thread must be a JSON object.")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return anthropicThreadParam{}, newAnthropicThreadInvalidRequestError("thread must be a valid JSON object.")
	}
	var param anthropicThreadParam
	if err := json.Unmarshal(trimmed, &param); err != nil {
		return anthropicThreadParam{}, newAnthropicThreadInvalidRequestError("thread has invalid field types.")
	}
	switch param.Type {
	case "create":
		if _, hasPrev := fields["previous_message_id"]; hasPrev && !isJSONNullOrEmpty(fields["previous_message_id"]) {
			return anthropicThreadParam{}, newAnthropicThreadInvalidRequestError("previous_message_id is not allowed when thread.type is create.")
		}
		return param, nil
	case "continue":
		if strings.TrimSpace(param.PreviousMessageID) == "" {
			return anthropicThreadParam{}, newAnthropicThreadInvalidRequestError("previous_message_id is required and must be a non-empty string when thread.type is continue.")
		}
		return param, nil
	default:
		return anthropicThreadParam{}, newAnthropicThreadInvalidRequestError("thread.type must be either 'create' or 'continue'.")
	}
}

// prepareAnthropicThreadRequest validates and materializes a
// message-threads-2026-08-12 request into a standard stateless Anthropic
// Messages request, returning an active session hook to capture the reply.
func (handlers *AnthropicHandlers) prepareAnthropicThreadRequest(
	ctx context.Context,
	genericReq *httpclient.Request,
	topFields map[string]json.RawMessage,
	betaTokens []string,
) (*anthropicThreadSession, *httpclient.Request, *anthropicThreadError) {
	threadParam, threadErr := parseAnthropicThreadParam(topFields["thread"])
	if threadErr != nil {
		return nil, nil, threadErr
	}

	scope, scoped := anthropicThreadScopeFromContext(ctx)
	fingerprint := computeAnthropicThreadFingerprint(topFields, betaTokens)
	topCacheControlBP := 0
	if cc, ok := topFields["cache_control"]; ok && !isJSONNullOrEmpty(cc) {
		topCacheControlBP = 1
	}

	var unsupported []string
	if hasUnsupportedCompaction(topFields, betaTokens) {
		unsupported = append(unsupported, "context_management")
	}

	if threadParam.Type == "create" {
		rawMessages, msgErr := parseMessagesArray(topFields["messages"])
		if msgErr != nil {
			return nil, nil, msgErr
		}
		if len(rawMessages) == 0 {
			return nil, nil, newAnthropicThreadInvalidRequestError("messages must be a non-empty array when creating a thread.")
		}

		storedMsgs, msgBP := buildStoredMessages(rawMessages)
		var systemRaw, toolsRaw json.RawMessage
		if sys, ok := topFields["system"]; ok && !isJSONNullOrEmpty(sys) {
			systemRaw = append(json.RawMessage(nil), bytes.TrimSpace(sys)...)
		}
		if tools, ok := topFields["tools"]; ok && !isJSONNullOrEmpty(tools) {
			toolsRaw = append(json.RawMessage(nil), bytes.TrimSpace(tools)...)
		}
		sysBP := countRawCacheControl(systemRaw)
		toolsBP := countRawCacheControl(toolsRaw)
		if topCacheControlBP+sysBP+toolsBP+msgBP > anthropicMaxCacheControlBreakpoints {
			unsupported = append(unsupported, "cache_control")
		}
		if len(unsupported) > 0 {
			return nil, nil, newAnthropicThreadUnsupportedRequestError(unsupported)
		}

		// Strip "thread" from body and message-threads-2026-08-12 from Anthropic-Beta.
		delete(topFields, "thread")
		outboundBody, err := json.Marshal(topFields)
		if err != nil {
			return nil, nil, newAnthropicThreadInvalidRequestError("failed to encode request body.")
		}

		materialized := cloneHTTPClientRequest(genericReq, outboundBody, stripAnthropicBetaToken(genericReq.Headers, anthropicMessageThreadsBeta))
		var root *anthropicThreadRoot
		if scoped && handlers.ThreadStore != nil {
			root = &anthropicThreadRoot{
				scope:             scope,
				system:            systemRaw,
				tools:             toolsRaw,
				systemBreakpoints: sysBP,
				toolsBreakpoints:  toolsBP,
				fingerprint:       fingerprint,
			}
		}
		session := &anthropicThreadSession{
			store:    handlers.ThreadStore,
			isCreate: true,
			root:     root,
			turnMsgs: storedMsgs,
		}
		return session, materialized, nil
	}

	// Continue turn: prohibit create-only fields (even when null).
	if _, hasSystem := topFields["system"]; hasSystem {
		return nil, nil, newAnthropicThreadInvalidRequestError("system cannot be specified on a thread continue request.")
	}
	if _, hasTools := topFields["tools"]; hasTools {
		return nil, nil, newAnthropicThreadInvalidRequestError("tools cannot be specified on a thread continue request.")
	}

	var rawNewMessages []json.RawMessage
	if msgRaw, exists := topFields["messages"]; exists {
		var msgErr *anthropicThreadError
		rawNewMessages, msgErr = parseMessagesArray(msgRaw)
		if msgErr != nil {
			return nil, nil, msgErr
		}
	}
	newStoredMsgs, newMsgBP := buildStoredMessages(rawNewMessages)

	if len(unsupported) > 0 {
		return nil, nil, newAnthropicThreadUnsupportedRequestError(unsupported)
	}

	if !scoped || handlers.ThreadStore == nil {
		return nil, nil, newAnthropicThreadNotFoundError()
	}

	var fixedBP int
	reservation, reserveErr := handlers.ThreadStore.reserveContinue(
		scope,
		threadParam.PreviousMessageID,
		fingerprint,
		func(root *anthropicThreadRoot, prevStopReason string) *anthropicThreadError {
			if len(rawNewMessages) == 0 && prevStopReason != "pause_turn" {
				return newAnthropicThreadInvalidRequestError("messages must be a non-empty array when continuing a thread unless the previous turn ended with pause_turn.")
			}
			fixedBP = topCacheControlBP + root.systemBreakpoints + root.toolsBreakpoints
			if fixedBP+newMsgBP > anthropicMaxCacheControlBreakpoints {
				return newAnthropicThreadUnsupportedRequestError([]string{"cache_control"})
			}
			return nil
		},
	)
	if reserveErr != nil {
		return nil, nil, reserveErr
	}

	// Materialize the full stateless request starting from the continue turn's
	// fields (so Free fields like max_tokens, stream, stop_sequences, metadata,
	// service_tier take the new turn's values).
	delete(topFields, "thread")
	if len(reservation.root.system) > 0 {
		topFields["system"] = reservation.root.system
	}
	if len(reservation.root.tools) > 0 {
		topFields["tools"] = reservation.root.tools
	}
	topFields["messages"] = materializeThreadMessages(reservation.stored, rawNewMessages, fixedBP, newMsgBP)

	outboundBody, err := json.Marshal(topFields)
	if err != nil {
		handlers.ThreadStore.releaseContinue(reservation.parent)
		return nil, nil, newAnthropicThreadInvalidRequestError("failed to encode materialized request body.")
	}

	materialized := cloneHTTPClientRequest(genericReq, outboundBody, stripAnthropicBetaToken(genericReq.Headers, anthropicMessageThreadsBeta))
	session := &anthropicThreadSession{
		store:    handlers.ThreadStore,
		isCreate: false,
		parent:   reservation.parent,
		turnMsgs: newStoredMsgs,
	}
	return session, materialized, nil
}

func cloneHTTPClientRequest(src *httpclient.Request, body []byte, headers http.Header) *httpclient.Request {
	copied := *src
	copied.Body = body
	copied.Headers = headers
	return &copied
}

func generateThreadMessageID() string {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	return "msg_" + hex.EncodeToString(raw[:])
}

func ensureThreadMessageID(id string) (string, bool) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" || trimmed == "msg_unknown" || !strings.HasPrefix(trimmed, "msg_") {
		return generateThreadMessageID(), true
	}
	return trimmed, false
}

func isAnthropicToolUseBlockType(blockType string) bool {
	return blockType == "tool_use" || strings.HasSuffix(blockType, "_tool_use")
}

func hasFallbackMessageIteration(usageRaw json.RawMessage) bool {
	if isJSONNullOrEmpty(usageRaw) {
		return false
	}
	iterations := gjson.GetBytes(usageRaw, "iterations")
	if !iterations.IsArray() {
		return false
	}
	for _, item := range iterations.Array() {
		if item.Get("type").String() == "fallback_message" {
			return true
		}
	}
	return false
}

func shouldSkipStoringTurn(stopReason string, lastBlockType string, fallback bool) bool {
	if stopReason == "refusal" {
		return true
	}
	if fallback {
		return true
	}
	if stopReason == "max_tokens" && isAnthropicToolUseBlockType(lastBlockType) {
		return true
	}
	return false
}

func buildAssistantStoredMessage(role string, contentBlocks []json.RawMessage) (anthropicStoredMessage, error) {
	if role == "" {
		role = "assistant"
	}
	if len(contentBlocks) == 0 {
		// Match open-harness-router: keep an empty assistant turn well-formed.
		contentBlocks = []json.RawMessage{json.RawMessage(`{"type":"text","text":""}`)}
	}
	var buf bytes.Buffer
	buf.WriteString(`{"role":`)
	roleJSON, err := json.Marshal(role)
	if err != nil {
		return anthropicStoredMessage{}, err
	}
	buf.Write(roleJSON)
	buf.WriteString(`,"content":[`)
	for i, block := range contentBlocks {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(block)
	}
	buf.WriteString(`]}`)
	raw := json.RawMessage(buf.Bytes())
	return anthropicStoredMessage{
		raw:           raw,
		cacheControls: countRawCacheControl(raw),
	}, nil
}

// anthropicThreadSession implements chatResponseHook to capture non-streaming
// and streaming responses, normalize missing/non-Anthropic message IDs, commit
// completed turns, and release in_flight reservations on abort.
type anthropicThreadSession struct {
	mu        sync.Mutex
	store     *AnthropicThreadStore
	isCreate  bool
	root      *anthropicThreadRoot
	parent    *anthropicThreadNode
	turnMsgs  []anthropicStoredMessage
	finalized bool
}

func (s *anthropicThreadSession) commit(messageID, stopReason string, assistantMsg anthropicStoredMessage) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finalized {
		s.mu.Unlock()
		return
	}
	s.finalized = true
	s.mu.Unlock()

	if s.store == nil {
		return
	}

	delta := make([]anthropicStoredMessage, 0, len(s.turnMsgs)+1)
	delta = append(delta, s.turnMsgs...)
	delta = append(delta, assistantMsg)

	if s.isCreate {
		if s.root != nil {
			s.store.commitCreate(s.root, messageID, delta, stopReason)
		}
		return
	}
	s.store.commitContinue(s.parent, messageID, delta, stopReason)
}

func (s *anthropicThreadSession) OnAbort() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.finalized {
		s.mu.Unlock()
		return
	}
	s.finalized = true
	s.mu.Unlock()

	if !s.isCreate && s.store != nil && s.parent != nil {
		s.store.releaseContinue(s.parent)
	}
}

func (s *anthropicThreadSession) OnResponse(resp *httpclient.Response) *httpclient.Response {
	if s == nil || resp == nil || resp.StatusCode != http.StatusOK || len(resp.Body) == 0 {
		s.OnAbort()
		return resp
	}

	var parsed struct {
		ID         string            `json:"id"`
		Role       string            `json:"role"`
		Content    []json.RawMessage `json:"content"`
		StopReason *string           `json:"stop_reason"`
		Usage      json.RawMessage   `json:"usage"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		s.OnAbort()
		return resp
	}

	msgID, replaced := ensureThreadMessageID(parsed.ID)
	if replaced {
		if patched, err := sjson.SetBytes(resp.Body, "id", msgID); err == nil {
			copied := *resp
			copied.Body = patched
			resp = &copied
		}
	}

	stopReason := lo.FromPtr(parsed.StopReason)
	lastBlockType := ""
	if len(parsed.Content) > 0 {
		lastBlockType = gjson.GetBytes(parsed.Content[len(parsed.Content)-1], "type").String()
	}
	if shouldSkipStoringTurn(stopReason, lastBlockType, hasFallbackMessageIteration(parsed.Usage)) {
		s.OnAbort()
		return resp
	}

	assistantMsg, err := buildAssistantStoredMessage(parsed.Role, parsed.Content)
	if err != nil {
		s.OnAbort()
		return resp
	}
	s.commit(msgID, stopReason, assistantMsg)
	return resp
}

func (s *anthropicThreadSession) WrapStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) streams.Stream[*httpclient.StreamEvent] {
	if s == nil || stream == nil {
		return stream
	}
	return &anthropicThreadCaptureStream{
		ctx:       ctx,
		stream:    stream,
		session:   s,
		assembler: newAnthropicSSEMessageAssembler(),
	}
}

// anthropicThreadCaptureStream relays SSE events unchanged (patching only a
// missing or non-Anthropic message_start.message.id), assembles the full
// assistant content losslessly, and commits on message_stop before yielding it
// to the response writer.
//
//nolint:containedctx // Needed to avoid committing after request cancellation.
type anthropicThreadCaptureStream struct {
	ctx       context.Context
	stream    streams.Stream[*httpclient.StreamEvent]
	session   *anthropicThreadSession
	assembler *anthropicSSEMessageAssembler
	current   *httpclient.StreamEvent
}

func (s *anthropicThreadCaptureStream) Next() bool {
	if !s.stream.Next() {
		s.session.OnAbort()
		return false
	}

	ev := s.stream.Current()
	if ev != nil {
		patched, completed := s.assembler.feed(ev)
		ev = patched
		if completed {
			if s.stream.Err() == nil && (s.ctx == nil || s.ctx.Err() == nil) {
				if msgID, stopReason, assistantMsg, ok := s.assembler.finalize(); ok {
					s.session.commit(msgID, stopReason, assistantMsg)
				} else {
					s.session.OnAbort()
				}
			} else {
				s.session.OnAbort()
			}
		}
	}
	s.current = ev
	return true
}

func (s *anthropicThreadCaptureStream) Current() *httpclient.StreamEvent { return s.current }
func (s *anthropicThreadCaptureStream) Err() error                       { return s.stream.Err() }

func (s *anthropicThreadCaptureStream) Close() error {
	s.session.OnAbort()
	return s.stream.Close()
}

func (s *anthropicThreadCaptureStream) Interrupt() error {
	if interruptible, ok := s.stream.(streams.Interruptible); ok {
		return interruptible.Interrupt()
	}
	return nil
}

type assembledContentBlock struct {
	blockType         string
	fields            map[string]json.RawMessage
	textBuf           strings.Builder
	hasTextDelta      bool
	thinkingBuf       strings.Builder
	hasThinkingDelta  bool
	signatureBuf      strings.Builder
	hasSignatureDelta bool
	partialJSONBuf    strings.Builder
	sawInputJSONDelta bool
	citations         []json.RawMessage
	stopped           bool
	invalidToolJSON   bool
}

// anthropicSSEMessageAssembler reconstructs an Anthropic assistant message from
// streaming SSE events without losing thinking signatures, redacted_thinking,
// citations, server_tool_use, or unknown block fields.
type anthropicSSEMessageAssembler struct {
	messageID  string
	role       string
	stopReason string
	sawError   bool
	fallback   bool
	blocks     map[int]*assembledContentBlock
}

func newAnthropicSSEMessageAssembler() *anthropicSSEMessageAssembler {
	return &anthropicSSEMessageAssembler{
		role:   "assistant",
		blocks: make(map[int]*assembledContentBlock),
	}
}

func (a *anthropicSSEMessageAssembler) feed(ev *httpclient.StreamEvent) (*httpclient.StreamEvent, bool) {
	if ev == nil {
		return nil, false
	}
	eventType := ev.Type
	if eventType == "" && len(ev.Data) > 0 {
		eventType = gjson.GetBytes(ev.Data, "type").String()
	}

	switch eventType {
	case "message_start":
		if role := gjson.GetBytes(ev.Data, "message.role").String(); role != "" {
			a.role = role
		}
		if usageRaw := gjson.GetBytes(ev.Data, "message.usage").Raw; usageRaw != "" {
			if hasFallbackMessageIteration(json.RawMessage(usageRaw)) {
				a.fallback = true
			}
		}
		rawID := gjson.GetBytes(ev.Data, "message.id").String()
		msgID, replaced := ensureThreadMessageID(rawID)
		a.messageID = msgID
		if replaced && json.Valid(ev.Data) {
			if patched, err := sjson.SetBytes(ev.Data, "message.id", msgID); err == nil {
				copied := *ev
				copied.Data = patched
				return &copied, false
			}
		}
	case "content_block_start":
		idxRes := gjson.GetBytes(ev.Data, "index")
		blockRes := gjson.GetBytes(ev.Data, "content_block")
		if idxRes.Type == gjson.Number && blockRes.IsObject() {
			a.openBlock(int(idxRes.Int()), []byte(blockRes.Raw))
		}
	case "content_block_delta":
		idxRes := gjson.GetBytes(ev.Data, "index")
		deltaRes := gjson.GetBytes(ev.Data, "delta")
		if idxRes.Type == gjson.Number && deltaRes.IsObject() {
			a.applyDelta(int(idxRes.Int()), deltaRes)
		}
	case "content_block_stop":
		idxRes := gjson.GetBytes(ev.Data, "index")
		if idxRes.Type == gjson.Number {
			if blk, ok := a.blocks[int(idxRes.Int())]; ok {
				blk.stopped = true
				if isAnthropicToolUseBlockType(blk.blockType) && blk.sawInputJSONDelta {
					trimmed := strings.TrimSpace(blk.partialJSONBuf.String())
					if trimmed != "" && !json.Valid([]byte(trimmed)) {
						blk.invalidToolJSON = true
					}
				}
			}
		}
	case "message_delta":
		if sr := gjson.GetBytes(ev.Data, "delta.stop_reason"); sr.Type == gjson.String {
			a.stopReason = sr.String()
		}
		if usageRaw := gjson.GetBytes(ev.Data, "usage").Raw; usageRaw != "" {
			if hasFallbackMessageIteration(json.RawMessage(usageRaw)) {
				a.fallback = true
			}
		}
	case "error":
		a.sawError = true
	case "message_stop":
		return ev, true
	}
	return ev, false
}

func (a *anthropicSSEMessageAssembler) openBlock(index int, raw []byte) {
	fields := make(map[string]json.RawMessage)
	_ = json.Unmarshal(raw, &fields)
	blk := &assembledContentBlock{
		blockType: gjson.GetBytes(raw, "type").String(),
		fields:    fields,
	}
	if textRes := gjson.GetBytes(raw, "text"); textRes.Type == gjson.String {
		blk.textBuf.WriteString(textRes.String())
	}
	if thinkRes := gjson.GetBytes(raw, "thinking"); thinkRes.Type == gjson.String {
		blk.thinkingBuf.WriteString(thinkRes.String())
	}
	if sigRes := gjson.GetBytes(raw, "signature"); sigRes.Type == gjson.String {
		blk.signatureBuf.WriteString(sigRes.String())
	}
	if citRes := gjson.GetBytes(raw, "citations"); citRes.IsArray() {
		for _, c := range citRes.Array() {
			blk.citations = append(blk.citations, json.RawMessage(c.Raw))
		}
	}
	a.blocks[index] = blk
}

func (a *anthropicSSEMessageAssembler) applyDelta(index int, delta gjson.Result) {
	blk, ok := a.blocks[index]
	if !ok {
		blk = &assembledContentBlock{
			blockType: "text",
			fields:    map[string]json.RawMessage{"type": json.RawMessage(`"text"`)},
		}
		a.blocks[index] = blk
	}

	switch delta.Get("type").String() {
	case "text_delta":
		if t := delta.Get("text"); t.Type == gjson.String {
			blk.textBuf.WriteString(t.String())
			blk.hasTextDelta = true
		}
	case "thinking_delta":
		if t := delta.Get("thinking"); t.Type == gjson.String {
			blk.blockType = "thinking"
			blk.fields["type"] = json.RawMessage(`"thinking"`)
			blk.thinkingBuf.WriteString(t.String())
			blk.hasThinkingDelta = true
		}
	case "signature_delta":
		if sig := delta.Get("signature"); sig.Type == gjson.String {
			blk.blockType = "thinking"
			blk.fields["type"] = json.RawMessage(`"thinking"`)
			blk.signatureBuf.WriteString(sig.String())
			blk.hasSignatureDelta = true
		}
	case "input_json_delta":
		if pj := delta.Get("partial_json"); pj.Type == gjson.String {
			blk.sawInputJSONDelta = true
			blk.partialJSONBuf.WriteString(pj.String())
		}
	case "citations_delta":
		if cit := delta.Get("citation"); cit.Exists() && cit.Type != gjson.Null {
			blk.citations = append(blk.citations, json.RawMessage(cit.Raw))
		}
	}
}

func (a *anthropicSSEMessageAssembler) finalize() (string, string, anthropicStoredMessage, bool) {
	if a.sawError || a.messageID == "" {
		return "", "", anthropicStoredMessage{}, false
	}

	indices := make([]int, 0, len(a.blocks))
	for idx := range a.blocks {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	lastBlockType := ""
	if len(indices) > 0 {
		lastBlockType = a.blocks[indices[len(indices)-1]].blockType
	}
	if shouldSkipStoringTurn(a.stopReason, lastBlockType, a.fallback) {
		return "", "", anthropicStoredMessage{}, false
	}

	blocks := make([]json.RawMessage, 0, len(indices))
	for _, idx := range indices {
		blk := a.blocks[idx]
		rawBlock, ok := blk.marshal()
		if !ok {
			return "", "", anthropicStoredMessage{}, false
		}
		blocks = append(blocks, rawBlock)
	}

	assistantMsg, err := buildAssistantStoredMessage(a.role, blocks)
	if err != nil {
		return "", "", anthropicStoredMessage{}, false
	}
	return a.messageID, a.stopReason, assistantMsg, true
}

func (b *assembledContentBlock) marshal() (json.RawMessage, bool) {
	fields := make(map[string]json.RawMessage, len(b.fields)+3)
	maps.Copy(fields, b.fields)

	switch {
	case b.blockType == "text":
		if b.hasTextDelta || fields["text"] == nil {
			encoded, err := json.Marshal(b.textBuf.String())
			if err != nil {
				return nil, false
			}
			fields["text"] = encoded
		}
		if len(b.citations) > 0 {
			encoded, err := json.Marshal(b.citations)
			if err != nil {
				return nil, false
			}
			fields["citations"] = encoded
		}
	case b.blockType == "thinking":
		// Always emit both "thinking" and "signature" so Gemini/Claude thinking
		// signature verification passes byte-for-byte even when thinking is "".
		encodedThinking, err := json.Marshal(b.thinkingBuf.String())
		if err != nil {
			return nil, false
		}
		encodedSignature, err := json.Marshal(b.signatureBuf.String())
		if err != nil {
			return nil, false
		}
		fields["thinking"] = encodedThinking
		fields["signature"] = encodedSignature
	case isAnthropicToolUseBlockType(b.blockType):
		if b.invalidToolJSON {
			return nil, false
		}
		if b.sawInputJSONDelta {
			trimmed := strings.TrimSpace(b.partialJSONBuf.String())
			if trimmed == "" {
				fields["input"] = json.RawMessage(`{}`)
			} else {
				if !json.Valid([]byte(trimmed)) {
					return nil, false
				}
				fields["input"] = json.RawMessage(trimmed)
			}
		} else if len(bytes.TrimSpace(fields["input"])) == 0 {
			fields["input"] = json.RawMessage(`{}`)
		}
	}

	data, err := json.Marshal(fields)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(data), true
}
