package contexts

import (
	"context"
	"net/http"

	"github.com/looplj/axonhub/llm/httpclient"
)

type channelCredentialAttempt struct {
	client *httpclient.HttpClient
	finish func(status int, failed, canceled bool)
}

func WithChannelCredentialEgress(ctx context.Context, key string, client *httpclient.HttpClient, finish func(int, bool, bool)) context.Context {
	c := getContainer(ctx)
	c.mu.Lock()
	c.ChannelAPIKey = &key
	c.credentialAttempt = channelCredentialAttempt{client: client, finish: finish}
	c.mu.Unlock()
	return withContainer(ctx, c)
}

func GetChannelCredentialEgress(ctx context.Context) (*httpclient.HttpClient, bool) {
	c := getContainer(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	client := c.credentialAttempt.client
	return client, client != nil
}

// GetChannelCredentialRawTransport returns the dedicated transport prepared with
// compression disabled by the egress pool. The pool owns its idle connections.
func GetChannelCredentialRawTransport(ctx context.Context) (*http.Transport, bool) {
	client, ok := GetChannelCredentialEgress(ctx)
	if !ok {
		return nil, false
	}
	transport, ok := client.GetNativeClient().Transport.(*http.Transport)
	return transport, ok
}

func FinishChannelCredentialAttempt(ctx context.Context, status int, failed, canceled bool) {
	c := getContainer(ctx)
	c.mu.Lock()
	finish := c.credentialAttempt.finish
	c.credentialAttempt.finish = nil
	c.mu.Unlock()
	if finish != nil {
		finish(status, failed, canceled)
	}
}

// TakeChannelCredentialAttempt atomically transfers ownership of the current
// context-bound attempt callback and egress client so the caller can bind them
// to an immutable attempt handle independent of later retries.
func TakeChannelCredentialAttempt(ctx context.Context) (string, *httpclient.HttpClient, func(int, bool, bool)) {
	c := getContainer(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	key := ""
	if c.ChannelAPIKey != nil {
		key = *c.ChannelAPIKey
	}
	client := c.credentialAttempt.client
	finish := c.credentialAttempt.finish
	c.credentialAttempt.finish = nil
	return key, client, finish
}

func ResetChannelCredentialAttempt(ctx context.Context) {
	FinishChannelCredentialAttempt(ctx, 0, false, true)
	c := getContainer(ctx)
	c.mu.Lock()
	c.ChannelAPIKey = nil
	c.credentialAttempt = channelCredentialAttempt{}
	c.mu.Unlock()
}

func ExcludeChannelCredential(ctx context.Context, channelID int, key string) {
	c := getContainer(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.excludedCredentials == nil {
		c.excludedCredentials = make(map[int]map[string]struct{})
	}
	if c.excludedCredentials[channelID] == nil {
		c.excludedCredentials[channelID] = make(map[string]struct{})
	}
	c.excludedCredentials[channelID][key] = struct{}{}
}

func ChannelCredentialExcluded(ctx context.Context, channelID int, key string) bool {
	c := getContainer(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, found := c.excludedCredentials[channelID][key]
	return found
}

// HasRequestContainer distinguishes a request that owns attempt finalization from
// callers using a provider only to obtain a key (e.g. discovery).
func HasRequestContainer(ctx context.Context) bool {
	_, ok := ctx.Value(containerContextKey).(*contextContainer)
	return ok
}
