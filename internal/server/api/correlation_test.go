package api

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestRawHTTPProxyCorrelationOwnership(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "ew-raw:fixture", r.Header.Get(httpclient.ClientRequestIDHeader))
		require.Empty(t, r.Header.Get("AH-Request-Id"))
		w.Header().Set(httpclient.ClientRequestIDHeader, "upstream-must-not-replace")
		w.Header().Set("AH-Request-Id", "upstream-must-not-replace")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"offline_error"}}`)
	}))
	defer upstream.Close()
	fixture := newRawHTTPRetryFixture(t, upstream, []string{"offline-upstream-key"}, true)
	handler := NewRawHTTPHandlers(RawHTTPHandlersParams{ChannelService: fixture.channels, Ent: fixture.db})
	router := gin.New()
	router.Use(middleware.WithLoggingTracing(tracing.Config{}))
	router.Use(func(c *gin.Context) {
		ctx := ent.NewContext(authz.WithTestBypass(c.Request.Context()), fixture.db)
		ctx = contexts.WithAPIKey(ctx, &ent.APIKey{ProjectID: 1})
		c.Request = c.Request.WithContext(ctx)
	})
	router.Any("/raw/:channelID/*path", handler.Proxy)
	server := httptest.NewServer(router)
	defer server.Close()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", server.URL, fixture.channel.ID), nil)
	require.NoError(t, err)
	req.Header.Set(httpclient.ClientRequestIDHeader, "ew-raw:fixture")
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.Equal(t, []string{"ew-raw:fixture"}, response.Header.Values(httpclient.ClientRequestIDHeader))
	require.Len(t, response.Header.Values("AH-Request-Id"), 1)
	require.Regexp(t, `^ar-[A-Za-z0-9-]+$`, response.Header.Get("AH-Request-Id"))
}

func TestProviderCannotOverrideCorrelationEcho(t *testing.T) {
	for _, full := range []bool{false, true} {
		dst := http.Header{httpclient.ClientRequestIDHeader: {"ew-effective"}, "Ah-Request-Id": {"ar-local"}}
		src := http.Header{"x-client-request-id": {"upstream"}, "AH-Request-Id": {"upstream"}, "X-Request-Id": {"provider-id"}}
		copyProviderResponseHeaders(dst, src, full)
		require.Equal(t, []string{"ew-effective"}, dst.Values(httpclient.ClientRequestIDHeader))
		require.Equal(t, "ar-local", dst.Get("AH-Request-Id"))
		require.Equal(t, "provider-id", dst.Get("X-Request-Id"))
	}
}
