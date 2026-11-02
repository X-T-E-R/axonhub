// Package decision carries the System One and OpenAI Decisions native JSON
// protocols through the common authenticated, routed and persisted pipeline.
package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/tidwall/sjson"
)

type Inbound struct{ Format llm.APIFormat }

func NewInbound(format llm.APIFormat) *Inbound { return &Inbound{Format: format} }
func (t *Inbound) APIFormat() llm.APIFormat    { return t.Format }

func (t *Inbound) TransformRequest(ctx context.Context, req *httpclient.Request) (*llm.Request, error) {
	if req == nil {
		return nil, invalid("request is nil")
	}
	contentType := strings.ToLower(req.Headers.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "application/json") {
		return nil, invalid("native decisions require application/json; System One experimental images/multipart are not supported")
	}
	fields, err := requestFields(req.Body, t.Format)
	if err != nil {
		return nil, err
	}
	var model string
	if json.Unmarshal(fields["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return nil, invalid("model is required")
	}
	if err := validateProfileHeaders(req.Headers, t.Format); err != nil {
		return nil, err
	}
	return &llm.Request{Model: model, RequestType: llm.RequestTypeDecision, APIFormat: t.Format, RawRequest: req,
		Decision: &llm.DecisionRequest{Body: append([]byte(nil), req.Body...)}}, nil
}

func (t *Inbound) TransformResponse(ctx context.Context, resp *llm.Response) (*httpclient.Response, error) {
	if resp == nil || resp.Decision == nil || !llm.DecisionFormatsCompatible(t.Format, resp.APIFormat) {
		return nil, fmt.Errorf("native decision response format mismatch")
	}
	body := append([]byte(nil), resp.Decision.Body...)
	// The common model mapper changes only Response.Model. Do not introduce a
	// model field into responses that never had one, or rewrite nested models.
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) == nil && len(fields["model"]) > 0 && resp.Model != "" {
		var err error
		body, err = sjson.SetBytes(body, "model", resp.Model)
		if err != nil {
			return nil, err
		}
	}
	return &httpclient.Response{StatusCode: resp.Decision.StatusCode, Body: body, Headers: resp.Decision.Headers.Clone()}, nil
}
func (t *Inbound) TransformError(ctx context.Context, err error) *httpclient.Error {
	if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
		return httpErr
	}
	return openai.NewInboundTransformer().TransformError(ctx, err)
}
func (t *Inbound) TransformStream(context.Context, streams.Stream[*llm.Response]) (streams.Stream[*httpclient.StreamEvent], error) {
	return nil, invalid("native decisions do not support streaming")
}
func (t *Inbound) AggregateStreamChunks(context.Context, []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return nil, llm.ResponseMeta{}, invalid("native decisions do not support streaming")
}

type Config struct {
	Format         llm.APIFormat
	BaseURL        string
	EndpointPath   string
	APIKeyProvider auth.APIKeyProvider
}
type Outbound struct{ config Config }

func NewOutbound(config Config) (*Outbound, error) {
	if !llm.IsDecisionAPIFormat(config.Format) {
		return nil, fmt.Errorf("unsupported native decision format %q", config.Format)
	}
	if config.BaseURL == "" || config.APIKeyProvider == nil {
		return nil, fmt.Errorf("native decision base URL and API key provider are required")
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.EndpointPath == "" {
		switch config.Format {
		case llm.APIFormatOpenAIDecisions:
			config.EndpointPath = "/decisions"
		default:
			config.EndpointPath = "/v1/systemone"
		}
	}
	return &Outbound{config: config}, nil
}
func (t *Outbound) APIFormat() llm.APIFormat { return t.config.Format }
func (t *Outbound) TransformRequest(ctx context.Context, req *llm.Request) (*httpclient.Request, error) {
	if req == nil || req.RequestType != llm.RequestTypeDecision || req.Decision == nil || !llm.DecisionFormatsCompatible(req.APIFormat, t.config.Format) {
		return nil, invalid("cross-protocol decision conversion is not supported")
	}
	if _, err := requestFields(req.Decision.Body, t.config.Format); err != nil {
		return nil, err
	}
	if req.RawRequest != nil {
		if err := validateProfileHeaders(req.RawRequest.Headers, t.config.Format); err != nil {
			return nil, err
		}
	}
	body, err := sjson.SetBytes(req.Decision.Body, "model", req.Model)
	if err != nil {
		return nil, invalid("cannot map model: %v", err)
	}
	headers := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json"}}
	// ZDR is a client policy, never silently remove it to obtain a success.
	if req.RawRequest != nil {
		for name, values := range req.RawRequest.Headers {
			if strings.EqualFold(name, "x-cmd-zdr") ||
				(t.config.Format == llm.APIFormatTypeSafeSystemOne && strings.HasPrefix(strings.ToLower(name), "typesafe-danger-zone-")) {
				headers[name] = append([]string(nil), values...)
			}
		}
	}
	return &httpclient.Request{Method: http.MethodPost, URL: t.config.BaseURL + t.config.EndpointPath,
		Headers: headers, Body: body, RequestType: llm.RequestTypeDecision.String(), APIFormat: t.config.Format.String(),
		Auth: &httpclient.AuthConfig{Type: httpclient.AuthTypeBearer, APIKey: t.config.APIKeyProvider.Get(ctx)}}, nil
}
func (t *Outbound) TransformResponse(ctx context.Context, resp *httpclient.Response) (*llm.Response, error) {
	if resp == nil {
		return nil, fmt.Errorf("native decision upstream response is nil")
	}
	if resp.StatusCode >= 400 {
		return nil, &httpclient.Error{StatusCode: resp.StatusCode, Body: resp.Body, Headers: resp.Headers}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(resp.Body, &fields) != nil || fields == nil {
		return nil, contractError("response must be a JSON object")
	}
	answers := fields["answers"]
	if llm.IsSystemOneAPIFormat(t.config.Format) {
		var value map[string]json.RawMessage
		if json.Unmarshal(answers, &value) != nil || value == nil {
			return nil, contractError("answers must be an object")
		}
	} else {
		var value []json.RawMessage
		if json.Unmarshal(answers, &value) != nil || value == nil {
			return nil, contractError("answers must be an array")
		}
	}
	var model, id string
	_ = json.Unmarshal(fields["model"], &model)
	_ = json.Unmarshal(fields["id"], &id)
	usage, err := parseUsage(fields["usage"])
	if err != nil {
		return nil, contractError("invalid usage: %v", err)
	}
	return &llm.Response{ID: id, Model: model, RequestType: llm.RequestTypeDecision, APIFormat: t.config.Format, Usage: usage,
		Decision: &llm.DecisionResponse{Body: append([]byte(nil), resp.Body...), StatusCode: resp.StatusCode, Headers: resp.Headers.Clone()}}, nil
}
func (t *Outbound) TransformError(ctx context.Context, err *httpclient.Error) *llm.ResponseError {
	result := (&openai.OutboundTransformer{}).TransformError(ctx, err)
	result.HTTPError = err
	if err != nil {
		result.RetryAfter = httpclient.RetryAfterHeader(err.Headers)
	}
	return result
}
func (t *Outbound) TransformStream(context.Context, *httpclient.Request, streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	return nil, invalid("native decisions do not support streaming")
}
func (t *Outbound) AggregateStreamChunks(context.Context, *httpclient.Request, []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return nil, llm.ResponseMeta{}, invalid("native decisions do not support streaming")
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", transformer.ErrInvalidRequest, fmt.Sprintf(format, args...))
}
func contractError(format string, args ...any) error {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "upstream_contract_error", "message": fmt.Sprintf(format, args...)}})
	return &httpclient.Error{StatusCode: http.StatusBadGateway, Body: body}
}

func validateProfileHeaders(headers http.Header, format llm.APIFormat) error {
	if llm.IsSystemOneAPIFormat(format) {
		if headers.Get("TypeSafe-Danger-Zone-Images") != "" {
			return invalid("System One experimental images are not supported")
		}
		if format == llm.APIFormatCommandCodeSystemOne {
			for name := range headers {
				if strings.HasPrefix(strings.ToLower(name), "typesafe-danger-zone-") {
					return invalid("Command Code TypeSafe experimental headers are not supported")
				}
			}
		}
	}
	return nil
}

// Decode only routing and supported-capability boundaries. The original bytes
// remain authoritative; no DTO roundtrip discards fields or converts numbers.
func requestFields(body []byte, format llm.APIFormat) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil || fields == nil {
		return nil, invalid("body must be a JSON object")
	}
	if stream, ok := fields["stream"]; ok && string(stream) != "false" && string(stream) != "null" {
		return nil, invalid("native decisions do not support streaming")
	}
	if llm.IsSystemOneAPIFormat(format) {
		if _, ok := fields["state"]; !ok {
			return nil, invalid("state is required")
		}
		if _, ok := fields["assets"]; ok {
			return nil, invalid("System One experimental assets/images are not supported")
		}
		var questions map[string]json.RawMessage
		if json.Unmarshal(fields["questions"], &questions) != nil || len(questions) == 0 {
			return nil, invalid("System One questions must be a nonempty object map")
		}
		if format == llm.APIFormatCommandCodeSystemOne && len(questions) > 100 {
			return nil, invalid("Command Code System One supports at most 100 questions")
		}
		for key, raw := range questions {
			var q struct {
				Type     string          `json:"type"`
				Criteria json.RawMessage `json:"criteria"`
			}
			if json.Unmarshal(raw, &q) != nil {
				return nil, invalid("question %q must be an object", key)
			}
			if q.Type == "box" {
				return nil, invalid("System One experimental box questions are not supported")
			}
			if format == llm.APIFormatCommandCodeSystemOne {
				switch q.Type {
				case "noul", "choice":
				case "score":
					var levels []json.RawMessage
					if json.Unmarshal(q.Criteria, &levels) != nil || len(levels) < 2 {
						return nil, invalid("Command Code score criteria require at least 2 levels")
					}
				default:
					return nil, invalid("Command Code supports only noul, choice and score questions")
				}
			}
		}
	} else if format == llm.APIFormatOpenAIDecisions {
		var inputString string
		var inputItems []json.RawMessage
		if json.Unmarshal(fields["input"], &inputString) != nil && (json.Unmarshal(fields["input"], &inputItems) != nil || inputItems == nil) {
			return nil, invalid("Decisions input must be a string or ordered array")
		}
		var questions []json.RawMessage
		if json.Unmarshal(fields["questions"], &questions) != nil || len(questions) < 1 || len(questions) > 400 {
			return nil, invalid("Decisions questions must be an array of 1 to 400 questions")
		}
		for i, raw := range questions {
			var q struct {
				Type    string `json:"type"`
				Choices []struct {
					Value json.RawMessage `json:"value"`
				} `json:"choices"`
				Levels []json.RawMessage `json:"levels"`
			}
			if json.Unmarshal(raw, &q) != nil {
				return nil, invalid("questions[%d] must be an object", i)
			}
			switch q.Type {
			case "predicate":
			case "choice":
				if len(q.Choices) < 2 || len(q.Choices) > 255 {
					return nil, invalid("questions[%d] requires 2 to 255 choices", i)
				}
				seen := map[string]bool{}
				for _, choice := range q.Choices {
					var value any
					if json.Unmarshal(choice.Value, &value) != nil {
						return nil, invalid("choice value is required")
					}
					switch value.(type) {
					case string, bool:
					default:
						return nil, invalid("choice value must be a string or boolean")
					}
					normalized, _ := json.Marshal(value)
					if seen[string(normalized)] {
						return nil, invalid("choice values must be typed-unique")
					}
					seen[string(normalized)] = true
				}
			case "score":
				if len(q.Levels) < 2 || len(q.Levels) > 10 {
					return nil, invalid("questions[%d] requires 2 to 10 levels", i)
				}
			default:
				return nil, invalid("unsupported Decisions question type %q", q.Type)
			}
		}
	} else {
		return nil, invalid("unsupported decision format %q", format)
	}
	return fields, nil
}

// Missing or partial counters remain unknown in the native response. Only a
// complete input/output pair can enter the existing non-null token ledger.
// The raw response is the per-request source for partial/missing usage.
func parseUsage(raw json.RawMessage) (*llm.Usage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var u struct {
		Input        *int64 `json:"input_tokens"`
		Output       *int64 `json:"output_tokens"`
		Total        *int64 `json:"total_tokens"`
		InputDetails *struct {
			Cached int64 `json:"cached_tokens"`
			Write  int64 `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
		OutputDetails *struct {
			Reasoning int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}
	if u.Input == nil || u.Output == nil {
		return nil, nil
	}
	if *u.Input < 0 || *u.Output < 0 || (u.Total != nil && *u.Total < 0) {
		return nil, fmt.Errorf("token counters cannot be negative")
	}
	usage := &llm.Usage{PromptTokens: *u.Input, CompletionTokens: *u.Output, TotalTokens: *u.Input + *u.Output}
	if u.Total != nil {
		usage.TotalTokens = *u.Total
	}
	if u.InputDetails != nil {
		usage.PromptTokensDetails = &llm.PromptTokensDetails{CachedTokens: u.InputDetails.Cached, WriteCachedTokens: u.InputDetails.Write}
	}
	if u.OutputDetails != nil {
		usage.CompletionTokensDetails = &llm.CompletionTokensDetails{ReasoningTokens: u.OutputDetails.Reasoning}
	}
	return usage, nil
}
