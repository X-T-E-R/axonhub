package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

type rawHTTPRetryFixture struct {
	db       *ent.Client
	channels *biz.ChannelService
	channel  *biz.Channel
	baseURL  string
}

func newRawHTTPRetryFixture(t *testing.T, upstream *httptest.Server, keys []string, disableRetries bool) *rawHTTPRetryFixture {
	t.Helper()
	db := enttest.NewEntClient(t, "sqlite3", "file:raw-http-retry-"+uuid.NewString()+"?mode=memory&_fk=1")
	channels := biz.NewChannelServiceForTest(db)
	t.Cleanup(func() {
		channels.Stop()
		db.Close()
	})
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))
	settings := &objects.ChannelSettings{
		DisableRetries: disableRetries,
		KeySelection:   &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
		Proxy:          &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
		RawHTTP:        &objects.ChannelRawHTTP{Enabled: true, Resource: "raw-resource", AuthType: "bearer"},
	}
	row := db.Channel.Create().SetName("raw retry fixture").SetType(channel.TypeOpenai).SetStatus(channel.StatusEnabled).
		SetBaseURL(upstream.URL + "/api").SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
		SetSupportedModels([]string{"raw-resource"}).SetDefaultTestModel("raw-resource").SetSettings(settings).SaveX(ctx)
	built, err := channels.OpenRawHTTPChannel(ctx, row)
	require.NoError(t, err)
	return &rawHTTPRetryFixture{db: db, channels: channels, channel: built, baseURL: row.BaseURL}
}

func (f *rawHTTPRetryFixture) handler(afterSelect func(context.Context, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := ent.NewContext(authz.WithTestBypass(r.Context()), f.db)
		ctx = contexts.EnsureContainer(ctx)
		key := f.channels.RawHTTPAPIKey(ctx, f.channel)
		if key == "" {
			http.Error(w, "no eligible credential", http.StatusServiceUnavailable)
			return
		}
		if afterSelect != nil {
			afterSelect(ctx, key)
		}
		retryTransport := &rawHTTPRetryTransport{
			channels:       f.channels,
			channel:        f.channel,
			defaultClient:  f.channel.HTTPClient,
			authType:       f.channel.Settings.RawHTTP.AuthType,
			authHeader:     f.channel.Settings.RawHTTP.AuthHeader,
			initialKey:     key,
			maxAttempts:    min(f.channel.RoutableCredentialCount(), rawHTTPMaxAttempts),
			disableRetries: f.channel.Settings.DisableRetries,
		}
		defer contexts.FinishChannelCredentialAttempt(ctx, 0, false, true)
		defer retryTransport.CloseIdleConnections()

		base, err := url.Parse(f.baseURL)
		if err != nil {
			http.Error(w, "invalid test target", http.StatusInternalServerError)
			return
		}
		proxy := &httputil.ReverseProxy{
			Transport:     retryTransport,
			FlushInterval: -1,
			Rewrite: func(p *httputil.ProxyRequest) {
				destination := *base
				suffix := strings.TrimPrefix(p.In.URL.Path, fmt.Sprintf("/raw/%d", f.channel.ID))
				destination.Path = strings.TrimRight(destination.Path, "/") + suffix
				destination.RawPath = ""
				destination.RawQuery = p.In.URL.RawQuery
				destination.ForceQuery = p.In.URL.ForceQuery
				p.Out.URL = &destination
				p.Out.Host = destination.Host
				p.Out.GetBody = nil
				p.Out.Trailer = nil
				for _, name := range middleware.DefaultAPIKeyConfig.Headers {
					p.Out.Header.Del(name)
				}
				setRawHTTPAuth(p.Out.Header, f.channel.Settings.RawHTTP.AuthType, f.channel.Settings.RawHTTP.AuthHeader, key)
			},
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "upstream connection failed", http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

func rawHTTPRetryClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{DisableCompression: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func rawHTTPRetryRequest(t *testing.T, client *http.Client, proxyURL string, channelID int, body io.Reader, length int64) (*http.Response, *http.Request) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search?x=%%2F", proxyURL, channelID), body)
	require.NoError(t, err)
	if length >= 0 {
		req.ContentLength = length
	}
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-API-Key", "client-secret")
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := client.Do(req)
	require.NoError(t, err)
	return resp, req
}

func TestRawHTTPRetryWithReverseProxyAndInformationalResponse(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var receivedKeys []string
	var receivedBodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		receivedKeys = append(receivedKeys, r.Header.Get("Authorization"))
		receivedBodies = append(receivedBodies, body)
		mu.Unlock()
		require.Equal(t, "/api/search", r.URL.Path)
		require.Equal(t, "x=%2F", r.URL.RawQuery)
		require.Empty(t, r.Header.Get("X-API-Key"))
		if call == 1 {
			w.Header().Set("Link", "</help>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del("Link")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("rejected credential"))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer upstream.Close()

	fixture := newRawHTTPRetryFixture(t, upstream, []string{"raw-key-a", "raw-key-b"}, false)
	proxy := httptest.NewServer(fixture.handler(nil))
	defer proxy.Close()
	client := rawHTTPRetryClient(t)

	var informational []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		informational = append(informational, code)
		return nil
	}}
	body := []byte(`{"opaque":true}`)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search?x=%%2F", proxy.URL, fixture.channel.ID), bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer client-secret")
	req.Header.Set("X-API-Key", "client-secret")
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(req)
	require.NoError(t, err)
	gotBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, body, gotBody)
	require.Empty(t, informational, "the failed credential's 103 response must not commit to the client")
	require.EqualValues(t, 2, calls.Load())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, receivedKeys, 2)
	require.NotEqual(t, receivedKeys[0], receivedKeys[1])
	require.Equal(t, body, receivedBodies[0])
	require.Equal(t, body, receivedBodies[1])
}

func TestRawHTTPRetryableStatusMatrixAndThreeAttemptLimit(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests, 432, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, err := io.Copy(io.Discard, r.Body)
				require.NoError(t, err)
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte("retryable"))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			}))
			defer upstream.Close()
			fixture := newRawHTTPRetryFixture(t, upstream, []string{"one", "two"}, false)
			proxy := httptest.NewServer(fixture.handler(nil))
			defer proxy.Close()
			resp, _ := rawHTTPRetryRequest(t, rawHTTPRetryClient(t), proxy.URL, fixture.channel.ID, strings.NewReader("body"), 4)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "ok", string(body))
			require.EqualValues(t, 2, calls.Load())
		})
	}

	var calls atomic.Int32
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		seen = append(seen, r.Header.Get("Authorization"))
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("try another"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("third credential"))
	}))
	defer upstream.Close()
	fixture := newRawHTTPRetryFixture(t, upstream, []string{"one", "two", "three", "four"}, false)
	proxy := httptest.NewServer(fixture.handler(nil))
	defer proxy.Close()
	resp, _ := rawHTTPRetryRequest(t, rawHTTPRetryClient(t), proxy.URL, fixture.channel.ID, strings.NewReader("body"), 4)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "third credential", string(body))
	require.EqualValues(t, 3, calls.Load(), "the initial request plus at most two retries are allowed")
	require.Len(t, seen, 3)
	require.Len(t, map[string]struct{}{seen[0]: {}, seen[1]: {}, seen[2]: {}}, 3)
}

func TestRawHTTPRetryPreservesLatestResponseWhenNoAlternate(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		require.NoError(t, err)
		w.Header().Set("X-Upstream-Failure", "latest")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("latest retryable response"))
	}))
	defer upstream.Close()
	keys := []string{"one", "two"}
	fixture := newRawHTTPRetryFixture(t, upstream, keys, false)
	proxy := httptest.NewServer(fixture.handler(func(ctx context.Context, selected string) {
		for _, key := range keys {
			if key != selected {
				contexts.ExcludeChannelCredential(ctx, fixture.channel.ID, key)
			}
		}
	}))
	defer proxy.Close()

	resp, _ := rawHTTPRetryRequest(t, rawHTTPRetryClient(t), proxy.URL, fixture.channel.ID, strings.NewReader("opaque"), 6)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "latest", resp.Header.Get("X-Upstream-Failure"))
	require.Equal(t, "latest retryable response", string(body))
	require.EqualValues(t, 1, calls.Load())
}

func TestRawHTTPRetryBodyBoundsAndDisableRetries(t *testing.T) {
	for _, tc := range []struct {
		name           string
		bodySize       int
		unknownLength  bool
		disableRetries bool
	}{
		{name: "known oversize", bodySize: rawHTTPRetryBodyLimit + 1},
		{name: "unknown oversize", bodySize: rawHTTPRetryBodyLimit + 257, unknownLength: true},
		{name: "disabled", bodySize: 512, disableRetries: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var err error
				gotBody, err = io.ReadAll(r.Body)
				require.NoError(t, err)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("rejected"))
			}))
			defer upstream.Close()
			fixture := newRawHTTPRetryFixture(t, upstream, []string{"one", "two"}, tc.disableRetries)
			proxy := httptest.NewServer(fixture.handler(nil))
			defer proxy.Close()
			body := bytes.Repeat([]byte("x"), tc.bodySize)
			var reader io.Reader = bytes.NewReader(body)
			length := int64(tc.bodySize)
			if tc.unknownLength {
				reader = rawHTTPUnknownLengthReader{Reader: bytes.NewReader(body)}
				length = -1
			}
			resp, _ := rawHTTPRetryRequest(t, rawHTTPRetryClient(t), proxy.URL, fixture.channel.ID, reader, length)
			_, err := io.Copy(io.Discard, resp.Body)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
			require.EqualValues(t, 1, calls.Load())
			require.Equal(t, body, gotBody, "retry suppression must preserve every request byte")
		})
	}
}

type rawHTTPUnknownLengthReader struct{ io.Reader }

func TestRawHTTPRetryAllowsRequestBodyAtLimit(t *testing.T) {
	var calls atomic.Int32
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		bodies = append(bodies, body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	fixture := newRawHTTPRetryFixture(t, upstream, []string{"one", "two"}, false)
	proxy := httptest.NewServer(fixture.handler(nil))
	defer proxy.Close()
	body := bytes.Repeat([]byte("m"), rawHTTPRetryBodyLimit)
	resp, _ := rawHTTPRetryRequest(t, rawHTTPRetryClient(t), proxy.URL, fixture.channel.ID, bytes.NewReader(body), int64(len(body)))
	result, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "ok", string(result))
	require.EqualValues(t, 2, calls.Load())
	require.Len(t, bodies, 2)
	require.Equal(t, body, bodies[0])
	require.Equal(t, body, bodies[1])
}

func TestRawHTTPRetryNetworkErrorBeforeHeaders(t *testing.T) {
	var calls atomic.Int32
	var keys []string
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		keys = append(keys, r.Header.Get("Authorization"))
		bodies = append(bodies, body)
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("recovered"))
	}))
	defer upstream.Close()
	fixture := newRawHTTPRetryFixture(t, upstream, []string{"one", "two"}, false)
	proxy := httptest.NewServer(fixture.handler(nil))
	defer proxy.Close()
	resp, _ := rawHTTPRetryRequest(t, rawHTTPRetryClient(t), proxy.URL, fixture.channel.ID, strings.NewReader("retryable body"), 14)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "recovered", string(body))
	require.EqualValues(t, 2, calls.Load())
	require.NotEqual(t, keys[0], keys[1])
	require.Equal(t, []byte("retryable body"), bodies[0])
	require.Equal(t, bodies[0], bodies[1])
}
