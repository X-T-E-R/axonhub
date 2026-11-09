package llm

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestExtractUpstreamErrorDetail_PreservesFieldsAndHeaders(t *testing.T) {
	body := []byte(`{
		"type": "error",
		"error": {
			"type": "invalid_request_error",
			"code": "context_length_exceeded",
			"message": "prompt is too long",
			"param": "messages",
			"details": [{"reason":"MAX_TOKENS","domain":"llm.provider","metadata":{"limit":"200000"}}]
		},
		"trace_id": "trace_abc123"
	}`)
	headers := http.Header{
		"X-Request-Id": {"req_header_ignored_when_body_has_trace"},
	}

	detail, ok := ExtractUpstreamErrorDetail(body, headers)
	require.True(t, ok)
	require.Equal(t, "prompt is too long", detail.Message)
	require.Equal(t, "invalid_request_error", detail.Type)
	require.Equal(t, "context_length_exceeded", detail.Code)
	require.Equal(t, "messages", detail.Param)
	require.Equal(t, "trace_abc123", detail.RequestID)
	require.JSONEq(t, `[{"reason":"MAX_TOKENS","domain":"llm.provider","metadata":{"limit":"200000"}}]`, string(detail.Details))
}

func TestExtractUpstreamErrorDetail_HeaderRequestIDFallback(t *testing.T) {
	body := []byte(`{"error":{"message":"rate limited","type":"rate_limit_error","code":429}}`)
	headers := http.Header{
		"Anthropic-Request-Id": {"req_anth_999"},
	}

	detail, ok := ExtractUpstreamErrorDetail(body, headers)
	require.True(t, ok)
	require.Equal(t, "rate limited", detail.Message)
	require.Equal(t, "rate_limit_error", detail.Type)
	require.Equal(t, "429", detail.Code)
	require.Equal(t, "req_anth_999", detail.RequestID)
}

func TestRawErrorForInbound_SameProtocolAndCrossProtocolGuard(t *testing.T) {
	rawBody := []byte(`{"type":"error","error":{"type":"invalid_request_error","code":"bad_tool","message":"invalid tool input","custom_field":"keep_me"},"request_id":"req_1","extra_top":{"nested":true}}`)
	rawHTTP := &httpclient.Error{
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       rawBody,
		Headers: http.Header{
			"Request-Id":    {"req_hdr_1"},
			"Retry-After":   {"15"},
			"Authorization": {"Bearer secret"},
			"Set-Cookie":    {"sid=secret"},
		},
	}

	respErr := EnrichResponseError(&ResponseError{
		StatusCode: http.StatusBadRequest,
	}, rawHTTP, APIFormatAnthropicMessage)

	// Same protocol (Anthropic -> Anthropic): preserves raw body and unknown fields, filters unsafe headers.
	sameProto := RawErrorForInbound(APIFormatAnthropicMessage, respErr)
	require.NotNil(t, sameProto)
	require.Equal(t, http.StatusBadRequest, sameProto.StatusCode)
	require.JSONEq(t, string(rawBody), string(sameProto.Body))
	require.Equal(t, "req_hdr_1", sameProto.Headers.Get("Request-Id"))
	require.Equal(t, "15", sameProto.Headers.Get("Retry-After"))
	require.Empty(t, sameProto.Headers.Get("Authorization"))
	require.Empty(t, sameProto.Headers.Get("Set-Cookie"))

	// Cross protocol (Anthropic -> OpenAI or Gemini): must NOT return incompatible raw body.
	require.Nil(t, RawErrorForInbound(APIFormatOpenAIChatCompletion, respErr))
	require.Nil(t, RawErrorForInbound(APIFormatGeminiContents, respErr))
}

func TestRawErrorForInbound_ArbitraryUnknownJSONShapeAndDiagnosticHeaders(t *testing.T) {
	// Arbitrary JSON object without error/message/code/type/status keys, containing
	// indentation, HTML-like characters, and 64-bit integer above IEEE-754 float53 precision.
	rawBody := []byte("{\n  \"reason\": \"backend <restarting> & draining\",\n  \"vendor_payload\": {\n    \"seq\": 9007199254740993\n  }\n}\n")
	rawHTTP := &httpclient.Error{
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Body:       rawBody,
		Headers: http.Header{
			"X-Trace-Id":        {"trace_hdr_999"},
			"X-Goog-Request-Id": {"goog_req_888"},
			"X-Amzn-Requestid":  {"amzn_req_777"},
			"Authorization":     {"Bearer secret"},
		},
	}

	respErr := EnrichResponseError(&ResponseError{
		StatusCode: http.StatusServiceUnavailable,
	}, rawHTTP, APIFormatOpenAIChatCompletion)

	sameProto := RawErrorForInbound(APIFormatOpenAIChatCompletion, respErr)
	require.NotNil(t, sameProto)
	require.Equal(t, rawBody, sameProto.Body)
	require.Equal(t, "trace_hdr_999", sameProto.Headers.Get("X-Trace-Id"))
	require.Equal(t, "goog_req_888", sameProto.Headers.Get("X-Goog-Request-Id"))
	require.Equal(t, "amzn_req_777", sameProto.Headers.Get("X-Amzn-Requestid"))
	require.Empty(t, sameProto.Headers.Get("Authorization"))
}
