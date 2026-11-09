package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

func transformOrchestratorError(ctx context.Context, err error, orch *orchestrator.ChatCompletionOrchestrator) *httpclient.Error {
	err = wrapQuotaExhaustedAsResponseError(err)
	if orch != nil {
		err = applyUpstreamErrorPolicy(ctx, err, orch.SystemService)
		return orch.Inbound.TransformError(ctx, err)
	}

	return &httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Body:       []byte(`{"error":{"message":"internal server error","type":"internal_server_error"}}`),
	}
}

func applyUpstreamErrorPolicy(ctx context.Context, err error, systemService *biz.SystemService) error {
	if err == nil || systemService == nil {
		return err
	}

	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return err
	}

	policy := systemService.RetryPolicyOrDefault(ctx).UpstreamErrorPolicy
	if policy.Mode == biz.UpstreamErrorModePassthrough || policy.Mode == "" {
		return err
	}

	if !pipeline.IsUpstreamError(err) {
		return err
	}

	message := biz.DefaultUpstreamErrorMessage
	if policy.Mode == biz.UpstreamErrorModeCustom {
		message = strings.TrimSpace(policy.CustomMessage)
		if message == "" {
			message = biz.DefaultUpstreamErrorMessage
		}
	}

	var respErr *llm.ResponseError
	if errors.As(err, &respErr) {
		if respErr.Detail.Code == errCodeQuotaExhausted || respErr.Detail.Type == errTypeQuotaExhausted {
			return err
		}

		return &llm.ResponseError{
			StatusCode: respErr.StatusCode,
			RetryAfter: respErr.RetryAfter,
			Detail: llm.ErrorDetail{
				Message:   message,
				Type:      firstNonEmpty(respErr.Detail.Type, "upstream_error"),
				Code:      respErr.Detail.Code,
				RequestID: respErr.Detail.RequestID,
			},
		}
	}

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) {
		return &llm.ResponseError{
			StatusCode: httpErr.StatusCode,
			RetryAfter: httpclient.RetryAfterHeader(httpErr.Headers),
			Detail: llm.ErrorDetail{
				Message:   message,
				Type:      upstreamErrorTypeFromHTTP(httpErr),
				Code:      upstreamErrorCodeFromHTTP(httpErr),
				RequestID: upstreamRequestIDFromHTTP(httpErr),
			},
		}
	}

	return &llm.ResponseError{
		StatusCode: http.StatusBadGateway,
		Detail: llm.ErrorDetail{
			Message: message,
			Type:    "upstream_error",
		},
	}
}

type upstreamErrorStream struct {
	stream        streams.Stream[*httpclient.StreamEvent]
	ctx           context.Context
	systemService *biz.SystemService
}

func newUpstreamErrorStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent], systemService *biz.SystemService) streams.Stream[*httpclient.StreamEvent] {
	if stream == nil || systemService == nil {
		return stream
	}

	return &upstreamErrorStream{
		stream:        stream,
		ctx:           ctx,
		systemService: systemService,
	}
}

func (s *upstreamErrorStream) Next() bool {
	return s.stream.Next()
}

func (s *upstreamErrorStream) Current() *httpclient.StreamEvent {
	cur := s.stream.Current()
	if cur == nil || len(cur.Data) == 0 {
		return cur
	}

	policy := s.systemService.RetryPolicyOrDefault(s.ctx).UpstreamErrorPolicy
	if policy.Mode == "" || policy.Mode == biz.UpstreamErrorModePassthrough {
		return cur
	}

	return rewriteStreamEventIfUpstreamError(cur, policy)
}

func rewriteStreamEventIfUpstreamError(cur *httpclient.StreamEvent, policy biz.UpstreamErrorPolicy) *httpclient.StreamEvent {
	trimmed := bytes.TrimSpace(cur.Data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) || !json.Valid(trimmed) {
		return cur
	}

	message := biz.DefaultUpstreamErrorMessage
	if policy.Mode == biz.UpstreamErrorModeCustom {
		if custom := strings.TrimSpace(policy.CustomMessage); custom != "" {
			message = custom
		}
	}

	root := gjson.ParseBytes(trimmed)
	eventType := cur.Type
	if eventType == "" {
		eventType = root.Get("type").String()
	}

	// 1. OpenAI Responses stream terminal failure event: response.failed
	if eventType == "response.failed" || root.Get("type").String() == "response.failed" {
		rewritten, err := sjson.SetBytes(cur.Data, "response.error.message", message)
		if err != nil {
			return cur
		}
		rewritten, _ = sjson.DeleteBytes(rewritten, "response.error.details")
		clone := *cur
		clone.Data = rewritten
		return &clone
	}

	// 2. OpenAI Responses stream error event: {"type":"error","message":"...","code":"..."}
	if (eventType == "error" || root.Get("type").String() == "error") && !root.Get("error").Exists() && root.Get("message").Exists() {
		rewritten, err := sjson.SetBytes(cur.Data, "message", message)
		if err != nil {
			return cur
		}
		rewritten, _ = sjson.DeleteBytes(rewritten, "details")
		clone := *cur
		clone.Data = rewritten
		return &clone
	}

	// 3. Raw pass-through or provider SSE error envelope: {"error": ...} or event: error
	errNode := root.Get("error")
	if !errNode.Exists() || errNode.Type == gjson.Null {
		if eventType != "error" && root.Get("event").String() != "error" {
			return cur
		}
	}

	detail, _ := llm.ExtractUpstreamErrorDetail(trimmed, nil)
	if detail.Code == errCodeQuotaExhausted || detail.Type == errTypeQuotaExhausted {
		return cur
	}

	errType := detail.Type
	if errType == "" {
		errType = "upstream_error"
	}

	errMap := map[string]any{
		"message": message,
		"type":    errType,
	}
	if detail.Code != "" {
		errMap["code"] = detail.Code
	}
	if detail.RequestID != "" {
		errMap["request_id"] = detail.RequestID
	}

	envelope := map[string]any{
		"error": errMap,
	}
	if root.Get("type").String() == "error" {
		envelope["type"] = "error"
		if detail.RequestID != "" {
			envelope["request_id"] = detail.RequestID
		}
	}

	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return cur
	}

	clone := *cur
	clone.Data = rewritten
	return &clone
}

func (s *upstreamErrorStream) Err() error {
	err := s.stream.Err()
	if err == nil {
		return nil
	}

	policy := s.systemService.RetryPolicyOrDefault(s.ctx).UpstreamErrorPolicy
	if policy.Mode != "" && policy.Mode != biz.UpstreamErrorModePassthrough {
		return applyUpstreamErrorPolicy(s.ctx, pipeline.WrapUpstreamError(err), s.systemService)
	}

	return err
}

func (s *upstreamErrorStream) Close() error {
	return s.stream.Close()
}

func upstreamErrorTypeFromHTTP(httpErr *httpclient.Error) string {
	if httpErr != nil {
		if detail, ok := llm.ExtractUpstreamErrorDetail(httpErr.Body, httpErr.Headers); ok && detail.Type != "" {
			return detail.Type
		}
	}

	return "upstream_error"
}

func upstreamErrorCodeFromHTTP(httpErr *httpclient.Error) string {
	if httpErr != nil {
		if detail, ok := llm.ExtractUpstreamErrorDetail(httpErr.Body, httpErr.Headers); ok && detail.Code != "" {
			return detail.Code
		}
	}

	return ""
}

func upstreamRequestIDFromHTTP(httpErr *httpclient.Error) string {
	if httpErr != nil {
		if detail, ok := llm.ExtractUpstreamErrorDetail(httpErr.Body, httpErr.Headers); ok && detail.RequestID != "" {
			return detail.RequestID
		}
		return llm.ExtractRequestIDFromHeaders(httpErr.Headers)
	}

	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
