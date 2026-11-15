package httpclient

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidClientRequestID(t *testing.T) {
	for _, value := range []string{"ew-123:abc._-", strings.Repeat("a", 128), "ar-46e596a3-a1bf-43fd-95f7-30c0664e4ea5"} {
		require.True(t, ValidClientRequestID(value), value)
	}
	for _, value := range []string{"", "not safe", "\r\nheader", "日本", strings.Repeat("a", 129), "value,other", "☃"} {
		require.False(t, ValidClientRequestID(value), value)
	}
}

func TestCorrelationHeadersPreserveOnlyLookupMetadata(t *testing.T) {
	headers := http.Header{"x-client-request-id": {"ew-ray"}, "Cf-Ray": {"ray-sin"}, "Ah-Request-Id": {"ar-local"}, "Authorization": {"private"}, "Cf-Access-Jwt-Assertion": {"private"}}
	kept := CorrelationHeaders(headers)
	require.Len(t, kept, 3)
	require.Equal(t, "ew-ray", kept.Get(ClientRequestIDHeader))
	kept[ClientRequestIDHeader][0] = "changed"
	require.Equal(t, "ew-ray", headers["x-client-request-id"][0])
	masked := MaskSensitiveHeaders(headers)
	require.Equal(t, []string{"******"}, masked["Cf-Access-Jwt-Assertion"])
}

func TestInboundCorrelationUsesCommonOutboundMerge(t *testing.T) {
	out := &Request{Headers: http.Header{ClientRequestIDHeader: {"channel-override"}}}
	in := &Request{Headers: http.Header{ClientRequestIDHeader: {"ew-ray"}, "Ah-Request-Id": {"ar-local"}, "Authorization": {"private"}, "Cf-Ray": {"ray"}}}
	merged := MergeInboundRequest(out, in)
	require.Equal(t, "ew-ray", merged.Headers.Get(ClientRequestIDHeader))
	require.Empty(t, merged.Headers.Get("Authorization"))
	require.Empty(t, merged.Headers.Get("AH-Request-Id"))
	require.Empty(t, merged.Headers.Get("Cf-Ray"))
}

func TestDirectTaskHTTPRequestsUseCurrentOperationCorrelation(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		ctx := WithClientRequestID(t.Context(), "ew-current-operation")
		req, err := BuildHttpRequest(ctx, &Request{Method: method, URL: "https://offline.invalid/tasks/task-1", Headers: http.Header{ClientRequestIDHeader: {"old-task-id"}}})
		require.NoError(t, err)
		require.Equal(t, "ew-current-operation", req.Header.Get(ClientRequestIDHeader))
	}
}
