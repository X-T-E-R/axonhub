package decision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

const systemRequest = `{"model":"public","state":{"n":9007199254740993,"nested":[false,null]},"questions":{"id.with.dot":{"type":"noul","instructions":{"deep":9007199254740993}},"choice":{"type":"choice","criteria":{"z":null,"a":{"json":true}}},"score":{"type":"score","criteria":["first","second"]}},"future":{"n":9007199254740993}}`
const openAIRequest = `{"model":"public","input":[{"role":"user","content":[{"type":"input_json","data":{"n":9007199254740993}},{"type":"input_image","image_url":"https://example.invalid/image.png","detail":"original"}]}],"questions":[{"type":"predicate","instructions":[{"type":"input_image","image_url":"data:image/png;base64,c3ludGhldGlj"}]},{"type":"choice","name":"typed","choices":[{"value":true},{"value":"true"},{"value":false}]},{"type":"score","levels":[{"label":"first"},{"label":"second"}]}],"prompt_cache_key":"key","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"future":9007199254740993}`

func TestNativeDecisionRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		format               llm.APIFormat
		body, path, response string
	}{
		{llm.APIFormatTypeSafeSystemOne, systemRequest, "/v1/systemone", `{"model":"actual","answers":{"id.with.dot":{"type":"noul","noul":0},"score":{"type":"score","score":0.25,"confidence":0,"legend":{"0":"first","1":"second"},"probabilities":{"0":0.75,"1":0.25}}},"usage":{"input_tokens":83,"output_tokens":43},"routing":{"n":9007199254740993},"x_jev_confidence":0.7}`},
		{llm.APIFormatCommandCodeSystemOne, systemRequest, "/v1/systemone", `{"model":"actual","answers":{},"usage":{"input_tokens":0,"output_tokens":0}}`},
		{llm.APIFormatOpenAIDecisions, openAIRequest, "/decisions", `{"model":"actual","answers":[{"type":"predicate","name":null,"probability":0},{"type":"choice","name":"typed","choice":true,"probabilities":[{"value":true,"probability":0},{"value":"true","probability":1}]},{"type":"refusal","name":null}],"usage":{"input_tokens":2048,"input_tokens_details":{"cached_tokens":1024,"cache_write_tokens":512},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2048},"future":9007199254740993}`},
	} {
		t.Run(tc.format.String(), func(t *testing.T) {
			inbound := NewInbound(tc.format)
			req, err := inbound.TransformRequest(t.Context(), &httpclient.Request{Body: []byte(tc.body), Headers: http.Header{"Content-Type": {"application/json"}}})
			require.NoError(t, err)
			require.Equal(t, llm.RequestTypeDecision, req.RequestType)
			require.Empty(t, req.Messages)
			req.Model = "actual"
			out, err := NewOutbound(Config{Format: tc.format, BaseURL: "https://synthetic.invalid/root", APIKeyProvider: auth.NewStaticKeyProvider("upstream-synthetic")})
			require.NoError(t, err)
			wire, err := out.TransformRequest(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, "https://synthetic.invalid/root"+tc.path, wire.URL)
			require.Equal(t, "upstream-synthetic", wire.Auth.APIKey)
			require.Contains(t, string(wire.Body), `9007199254740993`)
			require.Contains(t, string(wire.Body), `"model":"actual"`)
			require.Contains(t, string(wire.Body), `"future"`)
			require.Equal(t, tc.body, string(req.Decision.Body))
			unified, err := out.TransformResponse(t.Context(), &httpclient.Response{StatusCode: 200, Body: []byte(tc.response), Headers: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"synthetic"}}})
			require.NoError(t, err)
			require.NotNil(t, unified.Decision)
			if tc.format == llm.APIFormatOpenAIDecisions {
				require.EqualValues(t, 2048, unified.Usage.TotalTokens)
				require.EqualValues(t, 0, unified.Usage.CompletionTokens)
				require.EqualValues(t, 512, unified.Usage.PromptTokensDetails.WriteCachedTokens)
			}
			final, err := inbound.TransformResponse(t.Context(), unified)
			require.NoError(t, err)
			require.Equal(t, tc.response, string(final.Body))
		})
	}
}
func TestNativeDecisionUsagePresence(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want *llm.Usage
	}{
		{"", nil}, {"null", nil}, {`{}`, nil}, {`{"input_tokens":12}`, nil},
		{`{"input_tokens":0,"output_tokens":0}`, &llm.Usage{}},
		{`{"input_tokens":12,"output_tokens":4}`, &llm.Usage{PromptTokens: 12, CompletionTokens: 4, TotalTokens: 16}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseUsage([]byte(tc.raw))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	_, err := parseUsage([]byte(`{"input_tokens":1.5,"output_tokens":0}`))
	require.Error(t, err)
}
func TestNativeDecisionRejectsUnsupportedBeforeSend(t *testing.T) {
	for _, body := range []string{`{"model":"m","state":"s","questions":{}}`, `{"model":"m","state":"s","questions":{"b":{"type":"box"}}}`, `{"model":"m","state":"s","assets":{},"questions":{"q":{"type":"noul"}}}`, `{"model":"m","state":"s","stream":true,"questions":{"q":{"type":"noul"}}}`} {
		_, err := NewInbound(llm.APIFormatTypeSafeSystemOne).TransformRequest(t.Context(), &httpclient.Request{Body: []byte(body)})
		require.Error(t, err)
	}
	_, err := NewInbound(llm.APIFormatTypeSafeSystemOne).TransformRequest(t.Context(), &httpclient.Request{Body: []byte(systemRequest), Headers: http.Header{"Content-Type": {"multipart/form-data; boundary=x"}}})
	require.ErrorContains(t, err, "multipart")
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(systemRequest), &fields))
	questions := map[string]any{}
	for i := 0; i < 101; i++ {
		questions[fmt.Sprint(i)] = map[string]string{"type": "noul"}
	}
	fields["questions"], _ = json.Marshal(questions)
	body, _ := json.Marshal(fields)
	_, err = NewInbound(llm.APIFormatTypeSafeSystemOne).TransformRequest(t.Context(), &httpclient.Request{Body: body})
	require.NoError(t, err)
	_, err = NewInbound(llm.APIFormatCommandCodeSystemOne).TransformRequest(t.Context(), &httpclient.Request{Body: body})
	require.ErrorContains(t, err, "100")
	out, err := NewOutbound(Config{Format: llm.APIFormatOpenAIDecisions, BaseURL: "https://example.invalid/v1", APIKeyProvider: auth.NewStaticKeyProvider("synthetic")})
	require.NoError(t, err)
	req, err := NewInbound(llm.APIFormatTypeSafeSystemOne).TransformRequest(t.Context(), &httpclient.Request{Body: []byte(systemRequest)})
	require.NoError(t, err)
	_, err = out.TransformRequest(t.Context(), req)
	require.ErrorContains(t, err, "cross-protocol")
}
func TestOpenAIQuestionsGA400TypedAndEmptyAnswers(t *testing.T) {
	for _, n := range []int{400, 401} {
		body := fmt.Sprintf(`{"model":"m","input":"s","questions":[%s]}`, strings.TrimSuffix(strings.Repeat(`{"type":"predicate"},`, n), ","))
		_, err := NewInbound(llm.APIFormatOpenAIDecisions).TransformRequest(t.Context(), &httpclient.Request{Body: []byte(body)})
		if n == 400 {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	_, err := NewInbound(llm.APIFormatOpenAIDecisions).TransformRequest(t.Context(), &httpclient.Request{Body: []byte(`{"model":"m","input":"s","questions":[{"type":"choice","choices":[{"value":true},{"value":true}]}]}`)})
	require.ErrorContains(t, err, "typed-unique")
}
func TestNativeErrorsRetainEnvelopeAndRetryAfter(t *testing.T) {
	for _, status := range []int{400, 422, 429, 502, 529} {
		original := &httpclient.Error{StatusCode: status, Body: []byte(`{"detail":[{"loc":["body","state"],"msg":"Field required","type":"missing"}]}`), Headers: http.Header{"Retry-After": {"3"}}}
		out := &Outbound{}
		normalized := out.TransformError(context.Background(), original)
		result := NewInbound(llm.APIFormatTypeSafeSystemOne).TransformError(t.Context(), fmt.Errorf("wrapped: %w", normalized))
		require.Same(t, original, result)
		require.True(t, errors.Is(normalized, original))
		require.Equal(t, "3", result.Headers.Get("Retry-After"))
	}
}

func TestNativeProfileHeadersAndResponseAbsence(t *testing.T) {
	headers := http.Header{"Typesafe-Danger-Zone-Future-Text": {"enabled"}, "X-Cmd-Zdr": {"1"}}
	req, err := NewInbound(llm.APIFormatTypeSafeSystemOne).TransformRequest(t.Context(), &httpclient.Request{Body: []byte(systemRequest), Headers: headers})
	require.NoError(t, err)
	direct, err := NewOutbound(Config{Format: llm.APIFormatTypeSafeSystemOne, BaseURL: "https://synthetic.invalid", APIKeyProvider: auth.NewStaticKeyProvider("key")})
	require.NoError(t, err)
	wire, err := direct.TransformRequest(t.Context(), req)
	require.NoError(t, err)
	require.Equal(t, "enabled", wire.Headers.Get("TypeSafe-Danger-Zone-Future-Text"))
	require.Equal(t, "1", wire.Headers.Get("X-Cmd-Zdr"))
	command, err := NewOutbound(Config{Format: llm.APIFormatCommandCodeSystemOne, BaseURL: "https://synthetic.invalid/provider", APIKeyProvider: auth.NewStaticKeyProvider("key")})
	require.NoError(t, err)
	_, err = command.TransformRequest(t.Context(), req)
	require.ErrorContains(t, err, "experimental headers")
	for _, body := range []string{`{"answers":{}}`, `{"answers":{},"usage":{"input_tokens":1}}`} {
		resp, err := direct.TransformResponse(t.Context(), &httpclient.Response{StatusCode: 200, Body: []byte(body)})
		require.NoError(t, err)
		require.Nil(t, resp.Usage)
		resp.Model = "public"
		final, err := NewInbound(llm.APIFormatTypeSafeSystemOne).TransformResponse(t.Context(), resp)
		require.NoError(t, err)
		require.Equal(t, body, string(final.Body)) // Do not invent missing model/usage.
	}
}
