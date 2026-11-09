package llm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/spf13/cast"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm/httpclient"
)

// ErrorFormatCompatible reports whether an upstream error in upstreamFormat can be
// returned verbatim (preserving its raw JSON body and unknown fields) to a client
// expecting inboundFormat.
func ErrorFormatCompatible(inboundFormat, upstreamFormat APIFormat) bool {
	if inboundFormat == "" || upstreamFormat == "" {
		return false
	}

	if inboundFormat == upstreamFormat {
		return true
	}

	if IsDecisionAPIFormat(inboundFormat) && IsDecisionAPIFormat(upstreamFormat) {
		return DecisionFormatsCompatible(inboundFormat, upstreamFormat)
	}

	return isOpenAIStandardErrorFormat(inboundFormat) && isOpenAIStandardErrorFormat(upstreamFormat)
}

func isOpenAIStandardErrorFormat(format APIFormat) bool {
	//nolint:exhaustive // Only OpenAI-family endpoints sharing {"error":{...}} envelope are grouped here.
	switch format {
	case APIFormatOpenAIChatCompletion,
		APIFormatOpenAICompletion,
		APIFormatOpenAIResponse,
		APIFormatOpenAIResponseCompact,
		APIFormatOpenAIImageGeneration,
		APIFormatOpenAIImageEdit,
		APIFormatOpenAIImageVariation,
		APIFormatOpenAIEmbedding,
		APIFormatOpenAIVideo,
		APIFormatOpenAISpeech,
		APIFormatOpenAITranscription,
		APIFormatOpenAITranslation,
		APIFormatOpenAIModeration,
		APIFormatJinaRerank,
		APIFormatJinaEmbedding:
		return true
	default:
		return false
	}
}

// IsStructuredJSONErrorBody reports whether body is a non-empty valid JSON object or array
// rather than an empty payload, empty container, or plain text/HTML.
func IsStructuredJSONErrorBody(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) < 2 || !json.Valid(trimmed) {
		return false
	}

	switch trimmed[0] {
	case '{':
		var fields map[string]json.RawMessage
		return json.Unmarshal(trimmed, &fields) == nil && len(fields) > 0
	case '[':
		var items []json.RawMessage
		return json.Unmarshal(trimmed, &items) == nil && len(items) > 0
	default:
		return false
	}
}

// RawErrorForInbound returns a cloned raw *httpclient.Error from llmErr when the upstream
// error format is compatible with inboundFormat and the raw body is a structured JSON error.
// It ensures any RetryAfter advice on llmErr is preserved in the returned Headers.
func RawErrorForInbound(inboundFormat APIFormat, llmErr *ResponseError) *httpclient.Error {
	if llmErr == nil {
		return nil
	}

	if !ErrorFormatCompatible(inboundFormat, llmErr.APIFormat) {
		return nil
	}

	if llmErr.HTTPError != nil {
		if !IsStructuredJSONErrorBody(llmErr.HTTPError.Body) {
			return nil
		}

		statusCode := llmErr.StatusCode
		if statusCode == 0 {
			statusCode = llmErr.HTTPError.StatusCode
		}

		status := llmErr.HTTPError.Status
		if status == "" && statusCode != 0 {
			status = http.StatusText(statusCode)
		}

		return &httpclient.Error{
			Method:     llmErr.HTTPError.Method,
			URL:        llmErr.HTTPError.URL,
			StatusCode: statusCode,
			Status:     status,
			Body:       append([]byte(nil), llmErr.HTTPError.Body...),
			Headers:    ErrorHeadersFromResponseError(llmErr),
		}
	}

	if len(llmErr.StreamErrorPayload) > 0 && IsStructuredJSONErrorBody(llmErr.StreamErrorPayload) {
		var status string
		if llmErr.StatusCode != 0 {
			status = http.StatusText(llmErr.StatusCode)
		}
		return &httpclient.Error{
			StatusCode: llmErr.StatusCode,
			Status:     status,
			Body:       append([]byte(nil), llmErr.StreamErrorPayload...),
			Headers:    ErrorHeadersFromResponseError(llmErr),
		}
	}

	return nil
}

// ErrorHeadersFromResponseError extracts safe public upstream headers and validated Retry-After
// advice from a ResponseError.
func ErrorHeadersFromResponseError(llmErr *ResponseError) http.Header {
	if llmErr == nil {
		return nil
	}

	var headers http.Header
	if llmErr.HTTPError != nil && len(llmErr.HTTPError.Headers) > 0 {
		connectionHeaders := make(map[string]struct{})
		for _, value := range llmErr.HTTPError.Headers.Values("Connection") {
			for token := range strings.SplitSeq(value, ",") {
				if key := http.CanonicalHeaderKey(strings.TrimSpace(token)); key != "" {
					connectionHeaders[key] = struct{}{}
				}
			}
		}

		for key, values := range llmErr.HTTPError.Headers {
			canonical := http.CanonicalHeaderKey(key)
			if _, connectionScoped := connectionHeaders[canonical]; connectionScoped {
				continue
			}
			if !isSafePublicErrorHeader(canonical) {
				continue
			}
			if headers == nil {
				headers = make(http.Header)
			}
			headers[canonical] = append([]string(nil), values...)
		}
	}

	retryAfter := llmErr.RetryAfter
	if retryAfter == "" && llmErr.HTTPError != nil {
		retryAfter = httpclient.RetryAfterHeader(llmErr.HTTPError.Headers)
	}
	if validated := httpclient.RetryAfterHeader(http.Header{"Retry-After": {retryAfter}}); validated != "" {
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set("Retry-After", validated)
	}

	if len(headers) == 0 {
		return nil
	}

	return headers
}

var publicDiagnosticIDHeaders = []string{
	"X-Request-Id",
	"Request-Id",
	"Anthropic-Request-Id",
	"X-Typesafe-Request-Id",
	"X-Amzn-Requestid",
	"X-Goog-Request-Id",
	"X-Trace-Id",
}

// IsPublicDiagnosticHeader reports whether canonicalKey is a recognized public
// request/trace correlation header or an end-to-end public protocol header.
func IsPublicDiagnosticHeader(canonicalKey string) bool {
	if canonicalKey == "" || httpclient.IsSensitiveHeader(canonicalKey) {
		return false
	}
	for _, key := range publicDiagnosticIDHeaders {
		if canonicalKey == key {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(canonicalKey), "x-codex-")
}

func isSafePublicErrorHeader(canonicalKey string) bool {
	return IsPublicDiagnosticHeader(canonicalKey)
}

// EnrichResponseError attaches the upstream APIFormat, original HTTPError, StatusCode,
// RetryAfter, and any missing ErrorDetail fields (such as Details, RequestID, Code, Param)
// extracted from rawErr without overwriting fields already populated by the transformer.
func EnrichResponseError(respErr *ResponseError, rawErr *httpclient.Error, apiFormat APIFormat) *ResponseError {
	if respErr == nil {
		if rawErr == nil {
			return nil
		}
		respErr = &ResponseError{
			StatusCode: rawErr.StatusCode,
		}
	}

	if apiFormat != "" && respErr.APIFormat == "" {
		respErr.APIFormat = apiFormat
	}

	if rawErr == nil {
		return respErr
	}

	if respErr.StatusCode == 0 {
		respErr.StatusCode = rawErr.StatusCode
	}

	if respErr.HTTPError == nil {
		respErr.HTTPError = rawErr
	}

	if respErr.RetryAfter == "" {
		respErr.RetryAfter = httpclient.RetryAfterHeader(rawErr.Headers)
	}

	extracted, ok := ExtractUpstreamErrorDetail(rawErr.Body, rawErr.Headers)
	if ok {
		if respErr.Detail.Message == "" {
			respErr.Detail.Message = extracted.Message
		}
		if respErr.Detail.Type == "" || (respErr.Detail.Type == "api_error" && extracted.Type != "" && extracted.Type != "error") {
			respErr.Detail.Type = extracted.Type
		}
		if respErr.Detail.Code == "" {
			respErr.Detail.Code = extracted.Code
		}
		if respErr.Detail.Param == "" {
			respErr.Detail.Param = extracted.Param
		}
		if respErr.Detail.RequestID == "" {
			respErr.Detail.RequestID = extracted.RequestID
		}
		if respErr.Detail.TraceID == "" {
			respErr.Detail.TraceID = extracted.TraceID
		}
		if len(respErr.Detail.Details) == 0 && len(extracted.Details) > 0 {
			respErr.Detail.Details = extracted.Details
		}
	} else if respErr.Detail.RequestID == "" {
		respErr.Detail.RequestID = ExtractRequestIDFromHeaders(rawErr.Headers)
	}

	return respErr
}

// ExtractRequestIDFromHeaders extracts a request or trace identifier from upstream HTTP headers.
func ExtractRequestIDFromHeaders(headers http.Header) string {
	if len(headers) == 0 {
		return ""
	}

	for _, key := range publicDiagnosticIDHeaders {
		if v := strings.TrimSpace(headers.Get(key)); v != "" {
			return v
		}
	}

	return ""
}

// ExtractUpstreamErrorDetail extracts common error fields (message, type, code, param,
// request_id/trace_id, details) from a JSON error body and optional HTTP headers.
func ExtractUpstreamErrorDetail(body []byte, headers http.Header) (ErrorDetail, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) < 2 || !json.Valid(trimmed) {
		return ErrorDetail{}, false
	}

	root := gjson.ParseBytes(trimmed)
	if root.IsArray() {
		arr := root.Array()
		if len(arr) == 0 {
			return ErrorDetail{}, false
		}
		root = arr[0]
	}
	if !root.IsObject() {
		return ErrorDetail{}, false
	}

	candidate := root
	if d := root.Get("data"); d.IsObject() && (root.Get("event").String() == "error" || d.Get("error").Exists()) {
		candidate = d
	}

	errNode := candidate.Get("error")
	if !errNode.Exists() || errNode.Type == gjson.Null {
		errNode = candidate.Get("errors")
	}
	if errNode.IsArray() {
		arr := errNode.Array()
		if len(arr) > 0 {
			errNode = arr[0]
		}
	}

	var detail ErrorDetail

	if errNode.IsObject() {
		detail.Message = firstNonEmptyGJSONString(
			errNode.Get("message"),
			errNode.Get("detail"),
			candidate.Get("message"),
			candidate.Get("detail"),
		)
		detail.Type = firstNonEmptyGJSONString(
			errNode.Get("type"),
			errNode.Get("status"),
		)
		if detail.Type == "" {
			if topType := candidate.Get("type").String(); topType != "" && topType != "error" {
				detail.Type = topType
			}
		}
		detail.Code = gjsonScalarToString(errNode.Get("code"))
		if detail.Code == "" {
			detail.Code = gjsonScalarToString(candidate.Get("code"))
		}
		detail.Param = firstNonEmptyGJSONString(
			errNode.Get("param"),
			candidate.Get("param"),
		)
		if detailsNode := errNode.Get("details"); detailsNode.Exists() && detailsNode.Type != gjson.Null {
			detail.Details = json.RawMessage(detailsNode.Raw)
		} else if detailsNode := candidate.Get("details"); detailsNode.Exists() && detailsNode.Type != gjson.Null {
			detail.Details = json.RawMessage(detailsNode.Raw)
		}
	} else {
		if errNode.Type == gjson.String && errNode.String() != "" {
			detail.Message = errNode.String()
		} else {
			detail.Message = firstNonEmptyGJSONString(
				candidate.Get("message"),
				candidate.Get("detail"),
			)
		}
		if topType := candidate.Get("type").String(); topType != "" && topType != "error" {
			detail.Type = topType
		} else if status := candidate.Get("status").String(); status != "" {
			detail.Type = status
		}
		detail.Code = gjsonScalarToString(candidate.Get("code"))
		detail.Param = candidate.Get("param").String()
		if detailsNode := candidate.Get("details"); detailsNode.Exists() && detailsNode.Type != gjson.Null {
			detail.Details = json.RawMessage(detailsNode.Raw)
		}
	}

	traceID := firstNonEmptyGJSONString(
		candidate.Get("trace_id"),
		errNode.Get("trace_id"),
		root.Get("trace_id"),
	)
	if traceID == "" && len(headers) > 0 {
		traceID = strings.TrimSpace(headers.Get("X-Trace-Id"))
	}

	detail.RequestID = firstNonEmptyGJSONString(
		candidate.Get("request_id"),
		errNode.Get("request_id"),
		root.Get("request_id"),
		candidate.Get("trace_id"),
		errNode.Get("trace_id"),
		root.Get("trace_id"),
	)
	if detail.RequestID == "" {
		detail.RequestID = ExtractRequestIDFromHeaders(headers)
	}
	if traceID != "" && traceID != detail.RequestID {
		detail.TraceID = traceID
	}

	matched := detail.Message != "" || detail.Type != "" || detail.Code != "" || len(detail.Details) > 0
	return detail, matched
}

func firstNonEmptyGJSONString(results ...gjson.Result) string {
	for _, r := range results {
		if r.Exists() && r.Type == gjson.String && r.String() != "" {
			return r.String()
		}
	}
	return ""
}

func gjsonScalarToString(r gjson.Result) string {
	if !r.Exists() || r.Type == gjson.Null || r.IsObject() || r.IsArray() {
		return ""
	}
	if r.Type == gjson.String {
		return r.String()
	}
	return cast.ToString(r.Value())
}
