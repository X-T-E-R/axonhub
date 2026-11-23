package biz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func TestExtractStructuredJSONErrorBoundaries(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		contentEncoding string
		want            StructuredJSONError
	}{
		{
			name: "Exa TEAM_BLOCKED top-level tag ignores error prose",
			body: `{"error":"Your team has been blocked from using /search","tag":"TEAM_BLOCKED"}`,
			want: StructuredJSONError{Tag: "TEAM_BLOCKED"},
		},
		{
			name: "nested error.code and error.type ignore message prose",
			body: `{"error":{"code":"insufficient_quota","type":"billing_error","message":"TEAM_BLOCKED"}}`,
			want: StructuredJSONError{Code: "insufficient_quota", Type: "billing_error"},
		},
		{
			name: "top-level numeric code ignores unlisted top-level type",
			body: `{"code":40301,"type":"auth_error","message":"ignored"}`,
			want: StructuredJSONError{Code: "40301"},
		},
		{
			name: "exact tag preserves leading and trailing spaces without trimming",
			body: `{"tag":" TEAM_BLOCKED "}`,
			want: StructuredJSONError{Tag: " TEAM_BLOCKED "},
		},
		{
			name: "prose-only error/message yields empty classification",
			body: `{"error":"TEAM_BLOCKED","message":"TEAM_BLOCKED"}`,
			want: StructuredJSONError{},
		},
		{
			name: "HTML 403 body yields empty",
			body: `<html><body>403 TEAM_BLOCKED</body></html>`,
			want: StructuredJSONError{},
		},
		{
			name: "JSON array yields empty",
			body: `[{"tag":"TEAM_BLOCKED"}]`,
			want: StructuredJSONError{},
		},
		{
			name: "malformed JSON yields empty",
			body: `{"tag":"TEAM_BLOCKED"`,
			want: StructuredJSONError{},
		},
		{
			name:            "compressed content encoding skipped",
			body:            `{"tag":"TEAM_BLOCKED"}`,
			contentEncoding: "gzip",
			want:            StructuredJSONError{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractStructuredJSONError([]byte(tc.body), tc.contentEncoding)
			require.Equal(t, tc.want, got)
		})
	}

	require.True(t, exprReferencesStructuredErrorField(`status == 403 && errorTag == "TEAM_BLOCKED"`))
	require.True(t, exprReferencesStructuredErrorField(`errorCode == "40301"`))
	require.True(t, exprReferencesStructuredErrorField(`errorType == "auth_error"`))
	require.False(t, exprReferencesStructuredErrorField(`status >= 400`))
	require.False(t, exprReferencesStructuredErrorField(`status >= 400 && "errorTag" != ""`))
	require.False(t, exprReferencesStructuredErrorField(`status >= 400 && 'errorCode' != ''`))
}

func TestUnifiedAPIKeyRecoveryClearsStaleMetadataDiagnosticAndGuardsLateFeedback(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer func() {
		svc.Stop()
		_ = client.Close()
	}()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled: true,
		AutoDisableChannel: AutoDisableChannel{
			Enabled:  true,
			Statuses: []AutoDisableChannelStatus{{Status: 432, Times: 1}},
		},
	}))

	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(30 * time.Minute)
	remaining := 42.5
	keys := []string{"key-good-1", "key-good-2", "key-legacy-stale", "key-stay-blocked", "key-archived"}
	idGood1 := objects.ChannelAPIKeyFingerprint("key-good-1")
	idGood2 := objects.ChannelAPIKeyFingerprint("key-good-2")
	idLegacy := objects.ChannelAPIKeyFingerprint("key-legacy-stale")
	idBlocked := objects.ChannelAPIKeyFingerprint("key-stay-blocked")
	idArchived := objects.ChannelAPIKeyFingerprint("key-archived")

	ch := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Exa Recovery Fixture").
		SetStatus(channel.StatusDisabled).
		SetErrorMessage("All API keys disabled (last error: 403)").
		SetBaseURL("https://api.exa.ai").
		SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{
			{Key: "key-good-1", DisabledAt: now, ErrorCode: 403, Reason: "Exa TEAM_BLOCKED: Forbidden"},
			{Key: "key-good-2", DisabledAt: now, ErrorCode: 403, Reason: "Exa TEAM_BLOCKED: Forbidden"},
			{Key: "key-stay-blocked", DisabledAt: now, ErrorCode: 403, Reason: "Exa TEAM_BLOCKED: Forbidden"},
		}).
		SetSupportedModels([]string{"exa-search"}).
		SetDefaultTestModel("exa-search").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				ArchivedKeys: []objects.ChannelArchivedAPIKey{{
					ID:         idArchived,
					MaskedKey:  objects.MaskChannelAPIKey("key-archived"),
					ArchivedAt: &now,
					Reason:     "archived permanently",
				}},
				KeyMetadata: []objects.ChannelKeyMetadata{
					{
						ID:              idGood1,
						MaskedKey:       objects.MaskChannelAPIKey("key-good-1"),
						Status:          objects.ChannelKeyStatusDisabled,
						LastCheckedAt:   &now,
						Success:         lo.ToPtr(false),
						FailureCount:    3,
						Reason:          "Forbidden",
						Balance:         remaining,
						Currency:        "USD",
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{Remaining: &remaining},
						Available:       lo.ToPtr(true),
						StatusCode:      403,
						MatchedPolicy:   "Exa TEAM_BLOCKED",
						Action:          "disable_key",
						NextCheckAt:     &future,
						BackoffAttempt:  2,
						History:         []objects.ChannelKeyHealthCheckHistoryEntry{{ID: "h1", CheckedAt: now, Success: false, StatusCode: 403}},
					},
					{
						ID:             idGood2,
						MaskedKey:      objects.MaskChannelAPIKey("key-good-2"),
						Status:         objects.ChannelKeyStatusDisabled,
						LastCheckedAt:  &now,
						Success:        lo.ToPtr(false),
						FailureCount:   1,
						Reason:         "Forbidden",
						StatusCode:     403,
						MatchedPolicy:  "Exa TEAM_BLOCKED",
						Action:         "disable_key",
						NextCheckAt:    &future,
						BackoffAttempt: 1,
						History:        []objects.ChannelKeyHealthCheckHistoryEntry{{ID: "h2", CheckedAt: now, Success: false, StatusCode: 403}},
					},
					{
						// Already absent from disabled_api_keys (legacy stale metadata).
						ID:             idLegacy,
						MaskedKey:      objects.MaskChannelAPIKey("key-legacy-stale"),
						Status:         objects.ChannelKeyStatusDisabled,
						LastCheckedAt:  &now,
						Success:        lo.ToPtr(false),
						FailureCount:   2,
						Reason:         "stale reason",
						StatusCode:     403,
						MatchedPolicy:  "legacy",
						Action:         "disable_key",
						NextCheckAt:    &future,
						BackoffAttempt: 2,
						History:        []objects.ChannelKeyHealthCheckHistoryEntry{{ID: "h3", CheckedAt: now, Success: false, StatusCode: 403}},
					},
					{
						ID:           idBlocked,
						MaskedKey:    objects.MaskChannelAPIKey("key-stay-blocked"),
						Status:       objects.ChannelKeyStatusDisabled,
						FailureCount: 4,
						Action:       "disable_key",
					},
					{
						ID:           idArchived,
						MaskedKey:    objects.MaskChannelAPIKey("key-archived"),
						Status:       objects.ChannelKeyStatusArchived,
						FailureCount: 5,
						Reason:       "archived permanently",
					},
				},
			},
		}).
		SaveX(ctx)

	// Seed runtime penalty, error counters, and an active pre-recovery in-flight attempt on key-good-1.
	svc.incrementAPIKeyFailureCount(ch.ID, "key-good-1", 432)
	svc.incrementAPIKeyFailureCount(ch.ID, "key-stay-blocked", 432)
	reqCtx := contexts.EnsureContainer(context.Background())
	svc.credentialRuntime.mu.Lock()
	preGen, preFinish := svc.credentialRuntime.startAttempt(reqCtx, ch.ID, idGood1, now)
	sGood1 := svc.credentialRuntime.state(credentialRuntimeKey{ch.ID, idGood1}, now)
	sGood1.cooldownUntil = future
	sGood1.failureRate = 0.8
	sBlocked := svc.credentialRuntime.state(credentialRuntimeKey{ch.ID, idBlocked}, now)
	sBlocked.cooldownUntil = future
	sBlocked.failureRate = 0.9
	svc.credentialRuntime.mu.Unlock()

	// 1. EnableSelectedAPIKeys on the 3 known-good keys (including legacy stale metadata key).
	require.NoError(t, svc.EnableSelectedAPIKeys(ctx, ch.ID, []string{"key-good-1", idGood2, "key-legacy-stale"}))

	updated := client.Channel.GetX(ctx, ch.ID)
	// Channel stays disabled (never silently auto-enabled), but stale all-keys-disabled diagnostic is cleared.
	require.Equal(t, channel.StatusDisabled, updated.Status)
	require.Nil(t, updated.ErrorMessage)
	// Only key-stay-blocked remains disabled.
	require.Len(t, updated.DisabledAPIKeys, 1)
	require.Equal(t, "key-stay-blocked", updated.DisabledAPIKeys[0].Key)

	metaByID := lo.SliceToMap(updated.Settings.KeyHealthCheck.KeyMetadata, func(m objects.ChannelKeyMetadata) (string, objects.ChannelKeyMetadata) {
		return m.ID, m
	})
	for _, id := range []string{idGood1, idGood2, idLegacy} {
		meta := metaByID[id]
		require.Equal(t, objects.ChannelKeyStatusActive, meta.Status, id)
		require.Nil(t, meta.Success, "admin recovery must not fabricate probe success=true for %s", id)
		require.Zero(t, meta.FailureCount, id)
		require.Empty(t, meta.Reason, id)
		require.Zero(t, meta.StatusCode, id)
		require.Empty(t, meta.MatchedPolicy, id)
		require.Empty(t, meta.Action, id)
		require.Nil(t, meta.NextCheckAt, id)
		require.Zero(t, meta.BackoffAttempt, id)
		require.Len(t, meta.History, 1, "history must be preserved for %s", id)
	}
	require.NotNil(t, metaByID[idGood1].BalanceSnapshot)
	require.InDelta(t, remaining, *metaByID[idGood1].BalanceSnapshot.Remaining, 1e-9)
	// Unselected blocked key and archived key remain untouched.
	require.Equal(t, objects.ChannelKeyStatusDisabled, metaByID[idBlocked].Status)
	require.Equal(t, 4, metaByID[idBlocked].FailureCount)
	require.Equal(t, objects.ChannelKeyStatusArchived, metaByID[idArchived].Status)
	require.Len(t, updated.Settings.KeyHealthCheck.ArchivedKeys, 1)

	// Runtime counters/cooldown reset for recovered keys only, preserving active inFlight=1.
	inFlight1, failRate1, _, cooldown1 := svc.CredentialRuntimeMetrics(ch.ID, "key-good-1")
	require.Equal(t, 1, inFlight1, "active in-flight lease must be preserved across recovery")
	require.Zero(t, failRate1)
	require.True(t, cooldown1.IsZero())
	require.Zero(t, svc.peekAPIKeyFailureCount(ch.ID, "key-good-1", 432))

	_, failRateBlocked, _, cooldownBlocked := svc.CredentialRuntimeMetrics(ch.ID, "key-stay-blocked")
	require.InDelta(t, 0.9, failRateBlocked, 1e-9)
	require.True(t, cooldownBlocked.After(now))
	require.Equal(t, 1, svc.peekAPIKeyFailureCount(ch.ID, "key-stay-blocked", 432))

	// 2. Pre-recovery late feedback must release inFlight without re-poisoning runtime or DB!
	preFinish(432, CredentialAttemptFailure)
	svc.ApplyRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		CredentialOwnerID:  ch.ID,
		APIKey:             "key-good-1",
		StartTime:          now,
		EndTime:            time.Now(),
		ResponseStatusCode: 432,
		HasRecoveryGen:     true,
		RecoveryGen:        preGen,
	})
	inFlight1, failRate1, _, cooldown1 = svc.CredentialRuntimeMetrics(ch.ID, "key-good-1")
	require.Zero(t, inFlight1, "pre-recovery attempt must still release its in-flight lease")
	require.Zero(t, failRate1, "pre-recovery failure must not re-poison failureRate")
	require.True(t, cooldown1.IsZero(), "pre-recovery failure must not re-apply cooldown")
	afterLate := client.Channel.GetX(ctx, ch.ID)
	require.Len(t, afterLate.DisabledAPIKeys, 1, "pre-recovery failure must not re-disable recovered key")

	// 3. A rolled-back transaction must NOT reset runtime penalties.
	errRollback := errors.New("forced rollback")
	err := svc.RunInTransaction(ctx, func(txCtx context.Context) error {
		if err := svc.EnableAPIKey(txCtx, ch.ID, "key-stay-blocked"); err != nil {
			return err
		}
		return errRollback
	})
	require.ErrorIs(t, err, errRollback)
	_, failRateBlocked, _, cooldownBlocked = svc.CredentialRuntimeMetrics(ch.ID, "key-stay-blocked")
	require.InDelta(t, 0.9, failRateBlocked, 1e-9, "rolled-back recovery must not clear runtime state")
	require.True(t, cooldownBlocked.After(now))

	// 4. Unrelated error_message must NOT be cleared by EnableAllAPIKeys.
	client.Channel.UpdateOneID(ch.ID).SetErrorMessage("Upstream TLS handshake error").SaveX(ctx)
	require.NoError(t, svc.EnableAllAPIKeys(ctx, ch.ID))
	afterAll := client.Channel.GetX(ctx, ch.ID)
	require.Empty(t, afterAll.DisabledAPIKeys)
	require.NotNil(t, afterAll.ErrorMessage)
	require.Equal(t, "Upstream TLS handshake error", *afterAll.ErrorMessage)
	// Archived key still stays archived even after EnableAllAPIKeys.
	require.Len(t, afterAll.Settings.KeyHealthCheck.ArchivedKeys, 1)
	afterAllMeta := lo.SliceToMap(afterAll.Settings.KeyHealthCheck.KeyMetadata, func(m objects.ChannelKeyMetadata) (string, objects.ChannelKeyMetadata) {
		return m.ID, m
	})
	require.Equal(t, objects.ChannelKeyStatusArchived, afterAllMeta[idArchived].Status)
	require.Equal(t, objects.ChannelKeyStatusActive, afterAllMeta[idBlocked].Status)

	// 5. Post-recovery fresh failure IS recorded normally.
	svc.credentialRuntime.mu.Lock()
	postGen, postFinish := svc.credentialRuntime.startAttempt(reqCtx, ch.ID, idGood1, time.Now())
	svc.credentialRuntime.mu.Unlock()
	postFinish(432, CredentialAttemptFailure)
	_, failRate1, _, cooldown1 = svc.CredentialRuntimeMetrics(ch.ID, "key-good-1")
	require.Greater(t, failRate1, 0.0)
	require.True(t, cooldown1.After(time.Now()))
	svc.ApplyRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		CredentialOwnerID:  ch.ID,
		APIKey:             "key-good-1",
		StartTime:          time.Now(),
		EndTime:            time.Now(),
		ResponseStatusCode: 432,
		HasRecoveryGen:     true,
		RecoveryGen:        postGen,
	})
	afterPost := client.Channel.GetX(ctx, ch.ID)
	require.Len(t, afterPost.DisabledAPIKeys, 1)
	require.Equal(t, "key-good-1", afterPost.DisabledAPIKeys[0].Key)
}

func TestApplyRequestFailurePolicyDetachesCanceledContext(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer func() {
		svc.Stop()
		_ = client.Close()
	}()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	require.NoError(t, svc.SystemService.SetRetryPolicy(setupCtx, &RetryPolicy{
		Enabled: true,
		AutoDisableChannel: AutoDisableChannel{
			Enabled:  true,
			Statuses: []AutoDisableChannelStatus{{Status: 432, Times: 1}},
		},
	}))

	ch := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Detached Policy Fixture").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.tavily.com").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"k1", "k2"}}).
		SetSupportedModels([]string{"tavily-search"}).
		SetDefaultTestModel("tavily-search").
		SaveX(setupCtx)

	canceledCtx, cancel := context.WithCancel(ent.NewContext(context.Background(), client))
	cancel()

	changed := svc.ApplyRequestFailurePolicy(canceledCtx, &PerformanceRecord{
		ChannelID:          ch.ID,
		CredentialOwnerID:  ch.ID,
		APIKey:             "k1",
		StartTime:          time.Now(),
		EndTime:            time.Now(),
		ResponseStatusCode: 432,
		RequestCompleted:   true,
	})
	require.True(t, changed)

	saved := client.Channel.GetX(setupCtx, ch.ID)
	require.Len(t, saved.DisabledAPIKeys, 1)
	require.Equal(t, "k1", saved.DisabledAPIKeys[0].Key)
	require.Equal(t, 432, saved.DisabledAPIKeys[0].ErrorCode)
}

func TestLatePreRecoveryTransportFailureDoesNotBlockProxy(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer func() {
		svc.Stop()
		_ = client.Close()
	}()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proxy := client.OutboundProxy.Create().
		SetName("recovery-guard-proxy").
		SetProxyURL("http://127.0.0.1:19999").
		SetStatus("enabled").
		SaveX(ctx)

	key := "key-proxy-1"
	keyID := objects.ChannelAPIKeyFingerprint(key)
	row := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Proxy Recovery Guard").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.exa.ai").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{key}}).
		SetSupportedModels([]string{"exa-search"}).
		SetDefaultTestModel("exa-search").
		SetSettings(&objects.ChannelSettings{
			RawHTTP: &objects.ChannelRawHTTP{Enabled: true, Resource: "exa-search", AuthType: "bearer"},
			CredentialPolicies: []objects.ChannelCredentialPolicy{{
				ID:               keyID,
				EgressMode:       objects.CredentialEgressModePinnedProxy,
				OutboundProxyID:  lo.ToPtr(proxy.ID),
				EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
			}},
		}).
		SaveX(ctx)

	built, err := svc.OpenRawHTTPChannel(ctx, row)
	require.NoError(t, err)

	reqCtx := contexts.EnsureContainer(ctx)
	attempt := svc.ReserveRawHTTPCredential(reqCtx, built, nil, true)
	require.NotNil(t, attempt)

	// Administratively recover the key while the pre-recovery attempt is in flight.
	require.NoError(t, svc.EnableAPIKey(ctx, row.ID, key))

	// Late transport failure (status=0) must release leases without blocking the proxy.
	attempt.Finish(0, CredentialAttemptFailure)

	inFlight, failRate, _, _ := svc.CredentialRuntimeMetrics(row.ID, key)
	require.Zero(t, inFlight)
	require.Zero(t, failRate)

	svc.outboundProxies.mu.Lock()
	blockedUntil := svc.outboundProxies.blockedUntil[proxy.ID]
	svc.outboundProxies.mu.Unlock()
	require.True(t, blockedUntil.IsZero(), "late pre-recovery transport failure must not temporarily block proxy")
}

func TestAllowReplayPreservationRevokedWhenProfileConditionsChange(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer func() {
		svc.Stop()
		_ = client.Close()
	}()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	original := objects.FailurePolicyProfile{
		ID:          "exa-team-blocked",
		Name:        "Exa TEAM_BLOCKED",
		AllowReplay: lo.ToPtr(true),
		Sources:     []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
		Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
			StatusCodes: []int{403},
			Expr:        `status == 403 && method == "POST" && path == "/search" && errorTag == "TEAM_BLOCKED"`,
		},
		Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
	}

	// 1. Channel settings merge: unchanged condition preserves AllowReplay; changed condition revokes it.
	current := &objects.ChannelSettings{
		FailurePolicy: &objects.ChannelFailurePolicy{
			Mode:        objects.ChannelFailurePolicyModeOverride,
			KeyProfiles: []objects.FailurePolicyProfile{original},
		},
	}
	unchangedEdit := original
	unchangedEdit.AllowReplay = nil
	unchangedEdit.Name = "Renamed Only"
	mergedSame := mergeChannelSettingsForUpdate(current, &objects.ChannelSettings{
		FailurePolicy: &objects.ChannelFailurePolicy{
			Mode:        objects.ChannelFailurePolicyModeOverride,
			KeyProfiles: []objects.FailurePolicyProfile{unchangedEdit},
		},
	})
	require.NotNil(t, mergedSame.FailurePolicy.KeyProfiles[0].AllowReplay)
	require.True(t, *mergedSame.FailurePolicy.KeyProfiles[0].AllowReplay)

	changedEdit := original
	changedEdit.AllowReplay = nil
	changedEdit.Conditions.Expr = `status == 403 && method == "POST" && path == "/charge" && errorTag == "TEAM_BLOCKED"`
	mergedChanged := mergeChannelSettingsForUpdate(current, &objects.ChannelSettings{
		FailurePolicy: &objects.ChannelFailurePolicy{
			Mode:        objects.ChannelFailurePolicyModeOverride,
			KeyProfiles: []objects.FailurePolicyProfile{changedEdit},
		},
	})
	require.Nil(t, mergedChanged.FailurePolicy.KeyProfiles[0].AllowReplay, "changing profile conditions must revoke omitted AllowReplay carryover")

	// 2. System RetryPolicy parity: unchanged profile preserves AllowReplay; changed condition revokes it.
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled:       true,
		FailurePolicy: objects.FailurePolicy{KeyProfiles: []objects.FailurePolicyProfile{original}},
	}))
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled:       true,
		FailurePolicy: objects.FailurePolicy{KeyProfiles: []objects.FailurePolicyProfile{unchangedEdit}},
	}))
	savedSame := svc.SystemService.RetryPolicyOrDefault(ctx)
	require.NotNil(t, savedSame.FailurePolicy.KeyProfiles[0].AllowReplay)
	require.True(t, *savedSame.FailurePolicy.KeyProfiles[0].AllowReplay)

	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled:       true,
		FailurePolicy: objects.FailurePolicy{KeyProfiles: []objects.FailurePolicyProfile{changedEdit}},
	}))
	savedChanged := svc.SystemService.RetryPolicyOrDefault(ctx)
	require.Nil(t, savedChanged.FailurePolicy.KeyProfiles[0].AllowReplay)
}

func TestTransactionalEnableAPIKeyFencesLateFeedbackAcrossCommit(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer func() {
		svc.Stop()
		_ = client.Close()
	}()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	require.NoError(t, svc.SystemService.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled: true,
		AutoDisableChannel: AutoDisableChannel{
			Enabled:  true,
			Statuses: []AutoDisableChannelStatus{{Status: 432, Times: 1}},
		},
	}))

	key := "tx-recovered-key"
	keyID := objects.ChannelAPIKeyFingerprint(key)
	ch := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Tx Recovery Fence").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.tavily.com").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{key, "backup-key"}}).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{{Key: key, DisabledAt: time.Now(), ErrorCode: 432}}).
		SetSupportedModels([]string{"tavily-search"}).
		SetDefaultTestModel("tavily-search").
		SaveX(ctx)

	reqCtx := contexts.EnsureContainer(ctx)
	svc.credentialRuntime.mu.Lock()
	preGen, preFinish := svc.credentialRuntime.startAttempt(reqCtx, ch.ID, keyID, time.Now())
	svc.credentialRuntime.mu.Unlock()
	defer preFinish(432, CredentialAttemptFailure)

	// Commit EnableAPIKey inside an Ent transaction (matching GraphQL Transactioner).
	require.NoError(t, svc.RunInTransaction(ctx, func(txCtx context.Context) error {
		return svc.EnableAPIKey(txCtx, ch.ID, key)
	}))

	// Late pre-recovery 432 arriving immediately after transactional commit must be rejected.
	changed := svc.ApplyRequestFailurePolicy(ctx, &PerformanceRecord{
		ChannelID:          ch.ID,
		CredentialOwnerID:  ch.ID,
		APIKey:             key,
		StartTime:          time.Now().Add(-time.Second),
		EndTime:            time.Now(),
		ResponseStatusCode: 432,
		HasRecoveryGen:     true,
		RecoveryGen:        preGen,
	})
	require.False(t, changed)
	saved := client.Channel.GetX(ctx, ch.ID)
	require.Empty(t, saved.DisabledAPIKeys, "transactional EnableAPIKey must fence late pre-recovery feedback across commit")
}

