package responses

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// Exercise the real HTTP error decoder and both Responses transformers, including
// streaming requests rejected before an upstream stream has opened.
func TestResponsesHTTPErrorRetryAfter(t *testing.T) {
	const body = `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Try again in 10 seconds."}}`
	for _, streaming := range []bool{false, true} {
		for _, tt := range []struct {
			name, value, want string
			status            int
		}{
			{name: "ten seconds", value: "10", want: "10", status: http.StatusTooManyRequests},
			{name: "service unavailable", value: "10", want: "10", status: http.StatusServiceUnavailable},
			{name: "HTTP date", value: "Wed, 21 Oct 2037 07:28:00 GMT", want: "Wed, 21 Oct 2037 07:28:00 GMT", status: http.StatusTooManyRequests},
			{name: "zero", value: "0", want: "0", status: http.StatusTooManyRequests},
			{name: "missing", status: http.StatusTooManyRequests},
			{name: "invalid", value: "soon", status: http.StatusTooManyRequests},
			{name: "negative", value: "-10", status: http.StatusTooManyRequests},
		} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, tt.name), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if tt.value != "" {
						w.Header().Set("Retry-After", tt.value)
					}
					w.Header().Set("Authorization", "fixture-secret")
					w.Header().Set("Set-Cookie", "session=fixture-secret")
					w.Header().Set("X-Upstream-Debug", "not-public")
					w.WriteHeader(tt.status)
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()
				inbound := NewInboundTransformer()
				outbound, err := NewOutboundTransformer(server.URL, "fixture")
				require.NoError(t, err)
				p := pipeline.NewFactory(httpclient.NewHttpClient()).Pipeline(inbound, outbound)
				_, err = p.Process(t.Context(), &httpclient.Request{
					Method: http.MethodPost,
					Body:   []byte(fmt.Sprintf(`{"model":"fixture","input":"hello","stream":%v}`, streaming)),
				})
				require.Error(t, err)
				require.True(t, pipeline.IsUpstreamError(err))
				response := inbound.TransformError(t.Context(), err)
				require.Equal(t, tt.status, response.StatusCode)
				require.JSONEq(t, body, string(response.Body))
				require.Equal(t, tt.want, response.Headers.Get("Retry-After"))
				if tt.want == "" {
					require.Empty(t, response.Headers)
				} else {
					require.Equal(t, http.Header{"Retry-After": {tt.want}}, response.Headers)
				}
				require.Equal(t, int32(1), calls.Load())
			})
		}
	}
}
