package orchestrator

import (
	"context"
	"fmt"
	"net/http"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

type channelAdmissionRejectionReason uint8

const (
	channelAdmissionRejectedNone channelAdmissionRejectionReason = iota
	channelAdmissionRejectedFormat
	channelAdmissionRejectedCodexUA
)

// ChannelAdmissionPolicySelector filters candidate channels using each channel's
// request-format gate (allowRequestFormats / denyRequestFormats) and official
// Codex User-Agent gate (codexOfficialClient).
//
// It evaluates already-parsed req.APIFormat and req.RawRequest.Headers on the
// hot path with zero body re-parsing, zero regex compilation, and zero DB queries.
type ChannelAdmissionPolicySelector struct {
	wrapped               CandidateSelector
	FormatFilteredCount   int
	CodexUAFilteredCount  int
	LastRejectedAPIFormat string
}

func WithChannelAdmissionPolicySelector(wrapped CandidateSelector) *ChannelAdmissionPolicySelector {
	return &ChannelAdmissionPolicySelector{wrapped: wrapped}
}

func (s *ChannelAdmissionPolicySelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(candidates) == 0 {
		return candidates, nil
	}

	apiFormat := ""
	var userAgent string
	var uaChecked, uaValid bool
	if req != nil {
		apiFormat = string(req.APIFormat)
	}

	var filtered []*ChannelModelsCandidate
	for i, c := range candidates {
		if c == nil || c.Channel == nil {
			if filtered == nil {
				filtered = make([]*ChannelModelsCandidate, 0, len(candidates))
				filtered = append(filtered, candidates[:i]...)
			}
			continue
		}
		if !c.Channel.AllowsRequestFormat(apiFormat) {
			if filtered == nil {
				filtered = make([]*ChannelModelsCandidate, 0, len(candidates))
				filtered = append(filtered, candidates[:i]...)
			}
			s.FormatFilteredCount++
			s.LastRejectedAPIFormat = apiFormat
			continue
		}
		if c.Channel.Policies.RequiresCodexOfficialClient() {
			if !uaChecked {
				if req != nil && req.RawRequest != nil && req.RawRequest.Headers != nil {
					userAgent = req.RawRequest.Headers.Get("User-Agent")
				}
				uaValid = objects.IsCodexOfficialClientRequestStrict(userAgent)
				uaChecked = true
			}
			if !uaValid {
				if filtered == nil {
					filtered = make([]*ChannelModelsCandidate, 0, len(candidates))
					filtered = append(filtered, candidates[:i]...)
				}
				s.CodexUAFilteredCount++
				continue
			}
		}
		if filtered != nil {
			filtered = append(filtered, c)
		}
	}
	if filtered == nil {
		return candidates, nil
	}

	return filtered, nil
}

func (s *ChannelAdmissionPolicySelector) RejectionError(req *llm.Request) error {
	model := ""
	apiFormat := s.LastRejectedAPIFormat
	if req != nil {
		model = req.Model
		if apiFormat == "" {
			apiFormat = string(req.APIFormat)
		}
	}
	if s.FormatFilteredCount > 0 && s.CodexUAFilteredCount == 0 {
		return NewUnsupportedRequestFormatError(model, apiFormat)
	}
	if s.CodexUAFilteredCount > 0 {
		return NewCodexClientRequiredError(model)
	}
	if s.FormatFilteredCount > 0 {
		return NewUnsupportedRequestFormatError(model, apiFormat)
	}
	return nil
}

func NewUnsupportedRequestFormatError(model, apiFormat string) *llm.ResponseError {
	msg := fmt.Sprintf("Request format %q is not allowed for model %q.", apiFormat, model)
	if apiFormat == "" {
		msg = fmt.Sprintf("Request format is not allowed for model %q.", model)
	}
	return &llm.ResponseError{
		StatusCode: http.StatusBadRequest,
		Detail: llm.ErrorDetail{
			Type:    "invalid_request_error",
			Code:    "unsupported_request_format",
			Message: msg,
		},
	}
}

func NewCodexClientRequiredError(model string) *llm.ResponseError {
	return &llm.ResponseError{
		StatusCode: http.StatusForbidden,
		Detail: llm.ErrorDetail{
			Type:    "invalid_request_error",
			Code:    "client_not_allowed",
			Message: fmt.Sprintf("Model %q requires an official Codex client User-Agent.", model),
		},
	}
}

func checkChannelClientAdmission(ch *biz.Channel, req *llm.Request) error {
	if ch == nil || ch.Channel == nil {
		return nil
	}
	apiFormat := ""
	model := ""
	if req != nil {
		apiFormat = string(req.APIFormat)
		model = req.Model
	}
	if !ch.AllowsRequestFormat(apiFormat) {
		return NewUnsupportedRequestFormatError(model, apiFormat)
	}
	if ch.Policies.RequiresCodexOfficialClient() {
		var ua string
		if req != nil && req.RawRequest != nil && req.RawRequest.Headers != nil {
			ua = req.RawRequest.Headers.Get("User-Agent")
		}
		if !objects.IsCodexOfficialClientRequestStrict(ua) {
			return NewCodexClientRequiredError(model)
		}
	}
	return nil
}

// enforceChannelClientAdmission provides an outbound defense-in-depth check
// (covering SpecifiedChannelSelector or custom selectors) before any upstream request is sent.
func enforceChannelClientAdmission(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return pipeline.OnRawRequest("channel-client-admission", func(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		ch := outbound.GetCurrentChannel()
		if ch == nil {
			return request, nil
		}
		if err := checkChannelClientAdmission(ch, outbound.state.LlmRequest); err != nil {
			return nil, err
		}
		return request, nil
	})
}
