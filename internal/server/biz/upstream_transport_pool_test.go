package biz

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

type transportPoolConnIDContextKey struct{}

func TestUpstreamTransportPool_PointerInequalityAndReuse(t *testing.T) {
	pool := &UpstreamTransportPool{}
	defer pool.CloseIdleConnections()

	base := httpclient.NewHttpClient()
	insecureBase := httpclient.NewHttpClient(httpclient.WithInsecureSkipVerify(true))

	// Different credentials against the exact same proxy URL must get separate clients and http.Transports.
	clientA := pool.Get("1:cred-a", "http://127.0.0.1:18080", base, false)
	clientB := pool.Get("1:cred-b", "http://127.0.0.1:18080", base, false)
	require.NotNil(t, clientA)
	require.NotNil(t, clientB)
	require.NotSame(t, clientA, clientB)
	require.NotSame(t, clientA.GetNativeClient(), clientB.GetNativeClient())
	require.NotSame(t, clientA.GetNativeClient().Transport, clientB.GetNativeClient().Transport)

	// Repeated lookups for the exact same tuple must reuse the pooled client.
	clientA2 := pool.Get("1:cred-a", "http://127.0.0.1:18080", base, false)
	require.Same(t, clientA, clientA2)

	// Different proxy URL, noReuse flag, or base TLS verify setting creates an isolated entry.
	clientOtherProxy := pool.Get("1:cred-a", "http://127.0.0.1:18081", base, false)
	clientNoReuse := pool.Get("1:cred-a", "http://127.0.0.1:18080", base, true)
	clientInsecure := pool.Get("1:cred-a", "http://127.0.0.1:18080", insecureBase, false)

	require.NotSame(t, clientA, clientOtherProxy)
	require.NotSame(t, clientA, clientNoReuse)
	require.NotSame(t, clientA, clientInsecure)

	trNoReuse, ok := clientNoReuse.GetNativeClient().Transport.(*http.Transport)
	require.True(t, ok)
	require.True(t, trNoReuse.DisableKeepAlives)

	trInsecure, ok := clientInsecure.GetNativeClient().Transport.(*http.Transport)
	require.True(t, ok)
	require.NotNil(t, trInsecure.TLSClientConfig)
	require.True(t, trInsecure.TLSClientConfig.InsecureSkipVerify)

	// CloseIdleConnections purges entries so a subsequent Get builds a fresh client.
	pool.CloseIdleConnections()
	clientAAfterPurge := pool.Get("1:cred-a", "http://127.0.0.1:18080", base, false)
	require.NotSame(t, clientA, clientAAfterPurge)
}

func TestUpstreamTransportPool_RealSocketSeparation_SameUpstream(t *testing.T) {
	var nextConnID atomic.Int64
	var mu sync.Mutex
	seenByHeader := make(map[string][]int64)

	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connID, ok := r.Context().Value(transportPoolConnIDContextKey{}).(int64)
		require.True(t, ok)

		cred := r.Header.Get("X-Cred")
		mu.Lock()
		seenByHeader[cred] = append(seenByHeader[cred], connID)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"ok":true}`))
		require.NoError(t, err)
	}))
	upstream.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, transportPoolConnIDContextKey{}, nextConnID.Add(1))
	}
	upstream.Start()
	defer upstream.Close()

	pool := &UpstreamTransportPool{}
	defer pool.CloseIdleConnections()

	clientA := pool.Get("1:cred-a", "", nil, false)
	clientB := pool.Get("1:cred-b", "", nil, false)
	require.NotSame(t, clientA, clientB)

	for range 2 {
		_, err := clientA.Do(t.Context(), &httpclient.Request{
			Method:  http.MethodGet,
			URL:     upstream.URL + "/v1/models",
			Headers: http.Header{"X-Cred": {"cred-a"}},
		})
		require.NoError(t, err)

		_, err = clientB.Do(t.Context(), &httpclient.Request{
			Method:  http.MethodGet,
			URL:     upstream.URL + "/v1/models",
			Headers: http.Header{"X-Cred": {"cred-b"}},
		})
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, seenByHeader["cred-a"], 2)
	require.Len(t, seenByHeader["cred-b"], 2)
	// Each credential keeps reusing its own socket, and the two credentials never share one.
	require.Equal(t, seenByHeader["cred-a"][0], seenByHeader["cred-a"][1])
	require.Equal(t, seenByHeader["cred-b"][0], seenByHeader["cred-b"][1])
	require.NotEqual(t, seenByHeader["cred-a"][0], seenByHeader["cred-b"][0])
}

func TestUpstreamTransportPool_RealSocketSeparation_SameProxy(t *testing.T) {
	var nextConnID atomic.Int64
	var mu sync.Mutex
	seenByHeader := make(map[string][]int64)

	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connID, ok := r.Context().Value(transportPoolConnIDContextKey{}).(int64)
		require.True(t, ok)

		cred := r.Header.Get("X-Cred")
		mu.Lock()
		seenByHeader[cred] = append(seenByHeader[cred], connID)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"ok":true}`))
		require.NoError(t, err)
	}))
	proxy.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return context.WithValue(ctx, transportPoolConnIDContextKey{}, nextConnID.Add(1))
	}
	proxy.Start()
	defer proxy.Close()

	pool := &UpstreamTransportPool{}
	defer pool.CloseIdleConnections()

	clientA := pool.Get("1:cred-a", proxy.URL, nil, false)
	clientB := pool.Get("1:cred-b", proxy.URL, nil, false)
	require.NotSame(t, clientA, clientB)

	for range 2 {
		_, err := clientA.Do(t.Context(), &httpclient.Request{
			Method:  http.MethodGet,
			URL:     "http://upstream.example/v1/chat/completions",
			Headers: http.Header{"X-Cred": {"cred-a"}},
		})
		require.NoError(t, err)

		_, err = clientB.Do(t.Context(), &httpclient.Request{
			Method:  http.MethodGet,
			URL:     "http://upstream.example/v1/chat/completions",
			Headers: http.Header{"X-Cred": {"cred-b"}},
		})
		require.NoError(t, err)
	}

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, seenByHeader["cred-a"], 2)
	require.Len(t, seenByHeader["cred-b"], 2)
	require.Equal(t, seenByHeader["cred-a"][0], seenByHeader["cred-a"][1])
	require.Equal(t, seenByHeader["cred-b"][0], seenByHeader["cred-b"][1])
	require.NotEqual(t, seenByHeader["cred-a"][0], seenByHeader["cred-b"][0])
}
