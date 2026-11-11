package orchestrator

import (
	"context"
	"fmt"
	"net/http"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// ChannelAdmissionPolicySelector filters candidate channels using each channel's
// request-format gate (allowRequestFormats / denyRequestFormats) and official
// Codex User-Agent gate (codexOfficialClient).
//
// In selectCandidates, it wraps inbound.state.CandidateSelector (covering DefaultSelector,
// SpecifiedChannelSelector, and any custom selector) and evaluates already-parsed
// req.APIFormat and req.RawRequest.Headers on the hot path with zero body re-parsing,
// zero regex compilation, and zero DB queries.
type ChannelAdmissionPolicySelector struct {
	wrapped                        CandidateSelector
	FormatFilteredCount            int
	FormatFilteredAllowsResponses  int
	CodexUAFilteredCount           int
	CodexUAFilteredAllowsResponses int
	LastRejectedAPIFormat          string
}

func WithChannelAdmissionPolicySelector(wrapped CandidateSelector) *ChannelAdmissionPolicySelector {
	return &ChannelAdmissionPolicySelector{wrapped: wrapped}
}

func channelPolicyAllowsResponses(ch *biz.Channel) bool {
	return ch != nil && (ch.AllowsRequestFormat(string(llm.APIFormatOpenAIResponse)) || ch.AllowsRequestFormat(string(llm.APIFormatOpenAIResponseCompact)))
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
			if channelPolicyAllowsResponses(c.Channel) {
				s.FormatFilteredAllowsResponses++
			}
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
				if channelPolicyAllowsResponses(c.Channel) {
					s.CodexUAFilteredAllowsResponses++
				}
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
		return newUnsupportedRequestFormatError(model, apiFormat, s.FormatFilteredAllowsResponses > 0)
	}
	if s.CodexUAFilteredCount > 0 {
		return newCodexClientRequiredError(model, s.CodexUAFilteredAllowsResponses > 0)
	}
	if s.FormatFilteredCount > 0 {
		return newUnsupportedRequestFormatError(model, apiFormat, s.FormatFilteredAllowsResponses > 0)
	}
	return nil
}

const (
	responsesFormatRejectionMessage = "Use the Responses API (/v1/responses). We recommend the official Codex client."
	codexClientResponsesMessage     = "This endpoint requires a Codex-compatible client. Try the official Codex client with the Responses API."
	codexClientGenericMessage       = "This endpoint requires a Codex-compatible client. Try the official Codex client."
)

func NewUnsupportedRequestFormatError(model, apiFormat string) *llm.ResponseError {
	return newUnsupportedRequestFormatError(model, apiFormat, false)
}

func newUnsupportedRequestFormatError(model, apiFormat string, allowsResponses bool) *llm.ResponseError {
	var msg string
	if allowsResponses {
		msg = responsesFormatRejectionMessage
	} else if apiFormat == "" {
		msg = fmt.Sprintf("Request format is not allowed for model %q.", model)
	} else {
		msg = fmt.Sprintf("Request format %q is not allowed for model %q.", apiFormat, model)
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
	return newCodexClientRequiredError(model, true)
}

func newCodexClientRequiredError(_ string, allowsResponses bool) *llm.ResponseError {
	msg := codexClientGenericMessage
	if allowsResponses {
		msg = codexClientResponsesMessage
	}
	return &llm.ResponseError{
		StatusCode: http.StatusForbidden,
		Detail: llm.ErrorDetail{
			Type:    "invalid_request_error",
			Code:    "client_not_allowed",
			Message: msg,
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
	allowsResponses := channelPolicyAllowsResponses(ch)
	if !ch.AllowsRequestFormat(apiFormat) {
		return newUnsupportedRequestFormatError(model, apiFormat, allowsResponses)
	}
	if ch.Policies.RequiresCodexOfficialClient() {
		var ua string
		if req != nil && req.RawRequest != nil && req.RawRequest.Headers != nil {
			ua = req.RawRequest.Headers.Get("User-Agent")
		}
		if !objects.IsCodexOfficialClientRequestStrict(ua) {
			return newCodexClientRequiredError(model, allowsResponses)
		}
	}
	return nil
}
