package httpclient

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHTTPJSONSurrogatesWireBoundary(t *testing.T) {
	const raw = ` {"z":9007199254740993,"s":"\ud83d...\udef0\ud83d\ud83d\udef0","pair":"\uD83D\uDEF0","literal":"\\ud83d","signature":"opaque","a":1} `
	const normalized = ` {"z":9007199254740993,"s":"\ufffd...\ufffd\ufffd\ud83d\udef0","pair":"\uD83D\uDEF0","literal":"\\ud83d","signature":"opaque","a":1} `
	for _, tc := range []struct{ media, override, want string }{
		{"application/json", "", normalized},
		{"application/problem+json; charset=utf-8", "", normalized},
		{"application/octet-stream", "application/json", normalized},
		{"application/json", "application/octet-stream", raw},
		{"multipart/form-data; boundary=test", "", raw},
		{"application/octet-stream", "", raw},
		{"", "", raw},
	} {
		t.Run(tc.media+"/"+tc.override, func(t *testing.T) {
			r := &Request{Method: http.MethodPost, URL: "http://test.invalid", Headers: http.Header{"Content-Type": {tc.media}}, ContentType: tc.override, Body: []byte(raw)}
			wire, err := BuildHttpRequest(t.Context(), r)
			require.NoError(t, err)
			got, err := io.ReadAll(wire.Body)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
			require.Equal(t, int64(len(tc.want)), wire.ContentLength)
			require.Equal(t, raw, string(r.Body))
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, normalized, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	_, err := NewHttpClient().Do(t.Context(), &Request{Method: http.MethodPost, URL: server.URL, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(raw)})
	require.NoError(t, err)
}
