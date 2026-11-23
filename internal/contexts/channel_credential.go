package contexts

import (
	"context"
	"net/http"
	"sync"

	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	SessionStateOwnershipSame      = "same"
	SessionStateOwnershipDifferent = "different"
	SessionStateOwnershipUnknown   = "unknown"
)

// SessionAffinityRequest holds the immutable original ingress request snapshot
// used across attempts for anchor extraction, continuation gating, and idRewriteRules.
type SessionAffinityRequest struct {
	ProjectID          int
	RouteKind          string
	RawHeaders         http.Header
	RawBody            []byte
	WeakAnchorHint     string
	PreviousResponseID string
}

// SessionAffinityDecision holds the per-attempt credential binding decision.
type SessionAffinityDecision struct {
	AffinityKey        string
	AnchorSource       string
	AnchorValue        string
	SelectedKeyID      string
	PreviousBoundKeyID string
	PreviousOwnerKeyID string
	StateOwnership     string
	ObservedRev        uint64
	Switched           bool
	BusyOverflow       bool
	RebindOnSuccess    bool

	confirmOnce     sync.Once
	onSuccess       func(responseID string)
	onResponseOwner func(responseID string)
}

func (d *SessionAffinityDecision) SetOnSuccess(fn func(responseID string)) {
	if d == nil {
		return
	}
	d.onSuccess = fn
}

func (d *SessionAffinityDecision) SetOnResponseOwner(fn func(responseID string)) {
	if d == nil {
		return
	}
	d.onResponseOwner = fn
}

func (d *SessionAffinityDecision) ConfirmSuccess(responseID string) {
	if d == nil {
		return
	}
	d.confirmOnce.Do(func() {
		if d.onSuccess != nil {
			d.onSuccess(responseID)
		}
	})
}

func (d *SessionAffinityDecision) RecordResponseOwner(responseID string) {
	if d == nil || responseID == "" {
		return
	}
	if d.onResponseOwner != nil {
		d.onResponseOwner(responseID)
	}
}

func WithSessionAffinityRequest(ctx context.Context, req *SessionAffinityRequest) context.Context {
	c := getContainer(ctx)
	c.mu.Lock()
	c.sessionAffinityRequest = req
	c.mu.Unlock()
	return withContainer(ctx, c)
}

func GetSessionAffinityRequest(ctx context.Context) (*SessionAffinityRequest, bool) {
	c := getContainer(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionAffinityRequest, c.sessionAffinityRequest != nil
}

func WithSessionAffinityDecision(ctx context.Context, dec *SessionAffinityDecision) context.Context {
	c := getContainer(ctx)
	c.mu.Lock()
	c.sessionAffinityDecision = dec
	c.mu.Unlock()
	return withContainer(ctx, c)
}

func GetSessionAffinityDecision(ctx context.Context) (*SessionAffinityDecision, bool) {
	c := getContainer(ctx)
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionAffinityDecision, c.sessionAffinityDecision != nil
}

func ConfirmSessionAffinitySuccess(ctx context.Context, responseID string) {
	c := getContainer(ctx)
	c.mu.RLock()
	dec := c.sessionAffinityDecision
	c.mu.RUnlock()
	if dec != nil {
		dec.ConfirmSuccess(responseID)
	}
}

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
	c.sessionAffinityDecision = nil
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
