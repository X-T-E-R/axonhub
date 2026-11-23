package biz

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func TestSessionAffinity_TenantIsolationAnchorsClaimBusyOverflowAndCASRebind(t *testing.T) {
	store := NewSessionAffinityStore()

	k1, k2, k3 := "sk-kimi-a", "sk-kimi-b", "sk-kimi-c"
	fp1 := objects.ChannelAPIKeyFingerprint(k1)
	fp2 := objects.ChannelAPIKeyFingerprint(k2)
	fp3 := objects.ChannelAPIKeyFingerprint(k3)

	chEnt := &ent.Channel{
		ID:          51,
		Name:        "kimi",
		Type:        channel.TypeMoonshotCoding,
		Credentials: objects.ChannelCredentials{APIKeys: []string{k1, k2, k3}},
		Settings: &objects.ChannelSettings{
			SessionAffinity: &objects.ChannelSessionAffinity{
				Enabled:    true,
				TTLSeconds: lo.ToPtr(600),
				Anchors:    []string{"header:x-session-id", "json:metadata.user_id"},
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{ID: fp1, MaxConcurrent: lo.ToPtr(1)},
				{ID: fp2, MaxConcurrent: lo.ToPtr(1)},
				{ID: fp3, MaxConcurrent: lo.ToPtr(1)},
			},
		},
	}

	compiled, err := CompileChannelSessionAffinity(chEnt.Settings.SessionAffinity)
	require.NoError(t, err)

	bizCh := &Channel{
		Channel:              chEnt,
		sessionAffinityStore: store,
	}
	provider := NewSessionAffinityKeyProvider(bizCh, compiled)

	// 1. Ordered anchor extraction + tenant isolation (Project 1 vs Project 2 with identical session header).
	ctxProj1 := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 101))
	ctxProj1 = contexts.WithSessionAffinityRequest(ctxProj1, &contexts.SessionAffinityRequest{
		ProjectID:  101,
		RouteKind:  "anthropic.messages",
		RawHeaders: http.Header{"X-Session-Id": []string{"sess-shared-42"}},
		RawBody:    []byte(`{"metadata":{"user_id":"u-fallback"}}`),
	})
	ctxProj2 := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 202))
	ctxProj2 = contexts.WithSessionAffinityRequest(ctxProj2, &contexts.SessionAffinityRequest{
		ProjectID:  202,
		RouteKind:  "anthropic.messages",
		RawHeaders: http.Header{"X-Session-Id": []string{"sess-shared-42"}},
		RawBody:    []byte(`{"metadata":{"user_id":"u-fallback"}}`),
	})

	// Project 1 claims a key on first attempt.
	key1 := provider.Get(ctxProj1)
	require.NotEmpty(t, key1)
	fpClaimed1 := objects.ChannelAPIKeyFingerprint(key1)
	dec1A, ok := contexts.GetSessionAffinityDecision(ctxProj1)
	require.True(t, ok)
	require.Equal(t, fpClaimed1, dec1A.SelectedKeyID)
	require.Equal(t, "header:x-session-id", dec1A.AnchorSource)
	require.NotEmpty(t, dec1A.AffinityKey)
	dec1A.ConfirmSuccess("resp-proj1-1")
	contexts.FinishChannelCredentialAttempt(ctxProj1, http.StatusOK, false, false)

	// Project 2 with the exact same header MUST get a different tenant-isolated AffinityKey!
	key2 := provider.Get(ctxProj2)
	require.NotEmpty(t, key2)
	dec2A, ok := contexts.GetSessionAffinityDecision(ctxProj2)
	require.True(t, ok)
	require.NotEqual(t, dec1A.AffinityKey, dec2A.AffinityKey, "different projects must NEVER share affinity slots")
	contexts.FinishChannelCredentialAttempt(ctxProj2, http.StatusOK, false, false)

	// 2. Subsequent turn for Project 1 stays sticky on fpClaimed1.
	ctx1B := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 101))
	ctx1B = contexts.WithSessionAffinityRequest(ctx1B, &contexts.SessionAffinityRequest{
		ProjectID:  101,
		RouteKind:  "anthropic.messages",
		RawHeaders: http.Header{"X-Session-Id": []string{"sess-shared-42"}},
	})
	key1B := provider.Get(ctx1B)
	require.Equal(t, key1, key1B, "sticky session must reuse pinned key")
	// Keep ctx1B in flight so fpClaimed1 is at MaxConcurrency=1!

	// 3. Busy overflow: fpClaimed1 is occupied at MaxConcurrency=1 -> next concurrent request
	// borrows another key WITHOUT rebinding the affinity slot!
	ctxBusy := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 101))
	ctxBusy = contexts.WithSessionAffinityRequest(ctxBusy, &contexts.SessionAffinityRequest{
		ProjectID:  101,
		RouteKind:  "anthropic.messages",
		RawHeaders: http.Header{"X-Session-Id": []string{"sess-shared-42"}},
	})
	borrowedKey := provider.Get(ctxBusy)
	require.NotEmpty(t, borrowedKey)
	require.NotEqual(t, key1, borrowedKey, "busy overflow must borrow another healthy key")
	decBusy, _ := contexts.GetSessionAffinityDecision(ctxBusy)
	require.True(t, decBusy.BusyOverflow)
	decBusy.ConfirmSuccess("")
	contexts.FinishChannelCredentialAttempt(ctxBusy, http.StatusOK, false, false)
	contexts.FinishChannelCredentialAttempt(ctx1B, http.StatusOK, false, false)

	// Verify the affinity slot STILL points to fpClaimed1 (NOT rebound by busy overflow)!
	slotKey, origRev, exists := store.GetSlot(dec1A.AffinityKey, time.Now())
	require.True(t, exists)
	require.Equal(t, fpClaimed1, slotKey, "busy overflow MUST NOT rebind affinity slot")

	// 4. Failover without upstream success MUST NOT rebind; ONLY ConfirmSuccess CAS-rebinds!
	ctxFail := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 101))
	ctxFail = contexts.WithSessionAffinityRequest(ctxFail, &contexts.SessionAffinityRequest{
		ProjectID:  101,
		RouteKind:  "anthropic.messages",
		RawHeaders: http.Header{"X-Session-Id": []string{"sess-shared-42"}},
	})
	attemptFail, decFail := provider.selectAttempt(ctxFail, map[string]struct{}{key1: {}})
	require.NotNil(t, attemptFail)
	failoverKey := attemptFail.Key
	require.NotEmpty(t, failoverKey)
	require.NotEqual(t, key1, failoverKey)
	fpFailover := objects.ChannelAPIKeyFingerprint(failoverKey)

	// Before ConfirmSuccess, slot MUST still point to fpClaimed1!
	slotBefore, _, _ := store.GetSlot(dec1A.AffinityKey, time.Now())
	require.Equal(t, fpClaimed1, slotBefore, "failed/unconfirmed attempt must NOT rebind slot")

	// Now confirm success -> CAS rebind with observedRev succeeds and advances Rev!
	require.NotNil(t, decFail)
	decFail.ConfirmSuccess("resp-failover-2")
	attemptFail.legacyFinishFunc()(http.StatusOK, false, false)

	slotAfter, newRev, _ := store.GetSlot(dec1A.AffinityKey, time.Now())
	require.Equal(t, fpFailover, slotAfter, "confirmed failover must CAS-rebind slot")
	require.Greater(t, newRev, origRev)

	// 5. Stale CAS rebind MUST be rejected without clobbering the newer owner.
	_, _, swapped := store.CompareAndSwap(dec1A.AffinityKey, origRev, fp3, time.Minute, time.Now())
	require.False(t, swapped)
	slotStill, _, _ := store.GetSlot(dec1A.AffinityKey, time.Now())
	require.Equal(t, fpFailover, slotStill)

	// 6. previous_response_id owner tracking prefers the key that created resp-proj1-1 (fpClaimed1).
	ctxCont := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 101))
	ctxCont = contexts.WithSessionAffinityRequest(ctxCont, &contexts.SessionAffinityRequest{
		ProjectID:          101,
		RouteKind:          "openai.responses",
		RawBody:            []byte(`{"previous_response_id":"resp-proj1-1","input":"hi"}`),
		PreviousResponseID: "resp-proj1-1",
	})
	contKey := provider.Get(ctxCont)
	require.Equal(t, key1, contKey, "previous_response_id must prefer original response owner key")
	decCont, _ := contexts.GetSessionAffinityDecision(ctxCont)
	require.Equal(t, fpClaimed1, decCont.PreviousOwnerKeyID)
	require.Equal(t, contexts.SessionStateOwnershipSame, decCont.StateOwnership)
	contexts.FinishChannelCredentialAttempt(ctxCont, http.StatusOK, false, false)

	// 7. Concurrent race stress on ClaimIfEmpty / CompareAndSwap / Touch / RecordResponseOwner.
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 303))
			c = contexts.WithSessionAffinityRequest(c, &contexts.SessionAffinityRequest{
				ProjectID:  303,
				RouteKind:  "openai.chat",
				RawHeaders: http.Header{"X-Session-Id": []string{"race-session"}},
			})
			k := provider.Get(c)
			if k != "" {
				if d, ok := contexts.GetSessionAffinityDecision(c); ok {
					d.ConfirmSuccess("resp-race")
				}
				contexts.FinishChannelCredentialAttempt(c, http.StatusOK, false, false)
			}
		}()
	}
	wg.Wait()
	require.NotEmpty(t, []string{fp1, fp2, fp3})
}

func TestSessionAffinity_WeakPrefixFallbackAndValidation(t *testing.T) {
	store := NewSessionAffinityStore()
	k1, k2 := "sk-prefix-1", "sk-prefix-2"
	chEnt := &ent.Channel{
		ID:          52,
		Name:        "prefix-test",
		Type:        channel.TypeOpenai,
		Credentials: objects.ChannelCredentials{APIKeys: []string{k1, k2}},
		Settings: &objects.ChannelSettings{
			SessionAffinity: &objects.ChannelSessionAffinity{
				Enabled:    true,
				TTLSeconds: lo.ToPtr(300),
				Anchors:    []string{"header:x-missing"},
			},
		},
	}
	compiled, err := CompileChannelSessionAffinity(chEnt.Settings.SessionAffinity)
	require.NoError(t, err)
	bizCh := &Channel{Channel: chEnt, sessionAffinityStore: store}
	provider := NewSessionAffinityKeyProvider(bizCh, compiled)

	// Missing configured header falls back to WeakAnchorHint (bounded prompt prefix).
	ctx1 := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 1))
	ctx1 = contexts.WithSessionAffinityRequest(ctx1, &contexts.SessionAffinityRequest{
		ProjectID:      1,
		RouteKind:      "openai.chat",
		WeakAnchorHint: "prompt-prefix-hash-abc",
	})
	got1 := provider.Get(ctx1)
	require.NotEmpty(t, got1)
	contexts.FinishChannelCredentialAttempt(ctx1, http.StatusOK, false, false)

	ctx2 := contexts.EnsureContainer(contexts.WithProjectID(context.Background(), 1))
	ctx2 = contexts.WithSessionAffinityRequest(ctx2, &contexts.SessionAffinityRequest{
		ProjectID:      1,
		RouteKind:      "openai.chat",
		WeakAnchorHint: "prompt-prefix-hash-abc",
	})
	got2 := provider.Get(ctx2)
	require.Equal(t, got1, got2, "weak prefix fallback must pin identical conversation prefixes")
	contexts.FinishChannelCredentialAttempt(ctx2, http.StatusOK, false, false)

	// Config validation tests:
	_, err = CompileChannelSessionAffinity(&objects.ChannelSessionAffinity{
		Enabled: true,
		Anchors: []string{"json:messages.*.id"}, // wildcard forbidden
	})
	require.Error(t, err)
	_, err = CompileChannelSessionAffinity(&objects.ChannelSessionAffinity{
		Enabled: true,
		Anchors: []string{"header:Authorization"}, // protected header forbidden
	})
	require.Error(t, err)
	_, err = CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "header:Authorization",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32,
	}})
	require.Error(t, err)
	_, err = CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "header:X-Api-Key",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32,
	}})
	require.Error(t, err)
	_, err = CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:previous_response_id",
		Op:     objects.ChannelIDRewriteOpDelete,
		When:   objects.ChannelIDRewriteWhenAlways,
	}})
	require.Error(t, err)
	_, err = CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:messages.#.id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32,
	}})
	require.Error(t, err)
}
