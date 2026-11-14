package biz

import (
	"net/http"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/looplj/axonhub/llm/httpclient"
)

type upstreamTransportKey struct {
	credentialID, proxyURL string
	insecure, noReuse      bool
}
type upstreamTransportEntry struct {
	client   *httpclient.HttpClient
	lastUsed time.Time
}

// UpstreamTransportPool owns independent connection pools, never just wrappers
// over a shared transport. Eviction closes idle connections, not active streams.
type UpstreamTransportPool struct {
	mu      sync.Mutex
	clients *lru.Cache[upstreamTransportKey, upstreamTransportEntry]
}

func (p *UpstreamTransportPool) Get(credentialID, proxyURL string, base *httpclient.HttpClient, noReuse bool) *httpclient.HttpClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients == nil {
		p.clients, _ = lru.NewWithEvict(512, func(_ upstreamTransportKey, e upstreamTransportEntry) { e.client.CloseIdleConnections() })
	}
	insecure := false
	if base != nil {
		if tr, ok := base.GetNativeClient().Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
			insecure = tr.TLSClientConfig.InsecureSkipVerify
		}
	}
	key := upstreamTransportKey{credentialID, proxyURL, insecure, noReuse}
	now := time.Now()
	// Lazy idle cleanup keeps the cache bounded without a service goroutine.
	for _, k := range p.clients.Keys() {
		e, _ := p.clients.Peek(k)
		if now.Sub(e.lastUsed) > 5*time.Minute {
			p.clients.Remove(k)
		} else {
			break
		}
	}
	if e, ok := p.clients.Get(key); ok {
		e.lastUsed = now
		p.clients.Add(key, e)
		return e.client
	}
	proxy := &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled}
	if proxyURL != "" {
		proxy.Type = httpclient.ProxyTypeURL
		proxy.URL = proxyURL
		proxy.DisableConnectionReuse = noReuse
	}
	client := httpclient.NewHttpClientWithProxy(proxy, httpclient.WithInsecureSkipVerify(insecure))
	// Do not transparently transform opaque Raw HTTP bodies. LLM callers still
	// receive ordinary identity-encoded HTTP responses through this same pool.
	client.GetNativeClient().Transport.(*http.Transport).DisableCompression = true
	p.clients.Add(key, upstreamTransportEntry{client, now})
	return client
}

func (p *UpstreamTransportPool) CloseIdleConnections() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clients != nil {
		p.clients.Purge()
	}
}
