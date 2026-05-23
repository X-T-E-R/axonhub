package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestChannelKeyHealthCheckDue(t *testing.T) {
	now := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	oldCheck := now.Add(-2 * time.Hour)
	recentCheck := now.Add(-10 * time.Minute)

	ch := &ent.Channel{
		Status: channel.StatusEnabled,
		Credentials: objects.ChannelCredentials{
			APIKeys: []string{"key-due"},
		},
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:         true,
				IntervalMinutes: 60,
			},
		},
	}

	require.True(t, channelKeyHealthCheckDue(ch, now))

	ch.Settings.KeyHealthCheck.KeyMetadata = []objects.ChannelKeyMetadata{{
		ID:            objects.ChannelAPIKeyFingerprint("key-due"),
		LastCheckedAt: &oldCheck,
	}}
	require.True(t, channelKeyHealthCheckDue(ch, now))

	ch.Settings.KeyHealthCheck.KeyMetadata[0].LastCheckedAt = &recentCheck
	require.False(t, channelKeyHealthCheckDue(ch, now))

	ch.Settings.KeyHealthCheck.Enabled = false
	require.False(t, channelKeyHealthCheckDue(ch, now))
}

func TestRunHTTPChannelKeyHealthCheck_StatusBalanceAndSecretInjection(t *testing.T) {
	const testKey = "sk-test-secret"

	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		require.NotContains(t, r.URL.String(), testKey)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10.50"}]}`))
	}))
	defer server.Close()

	svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
	ch := &ent.Channel{
		BaseURL: server.URL,
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{},
		},
	}

	result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, testKey, objects.ChannelKeyHealthCheckHTTPRule{
		Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
		URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
		Path:             "/user/balance",
		ExpectedStatuses: []int{http.StatusOK},
	})

	require.True(t, result.Success, result.Reason)
	require.Equal(t, "Bearer "+testKey, gotAuthorization)
	require.Equal(t, "10.50", result.Balance)
	require.Equal(t, "CNY", result.Currency)
	require.NotNil(t, result.Available)
	require.True(t, *result.Available)
	require.NotContains(t, result.Reason, testKey)
}

func TestRunHTTPChannelKeyHealthCheck_DeepSeekDefaultRuleUsesBalanceEndpoint(t *testing.T) {
	const testKey = "sk-deepseek-secret"

	var gotPath string
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"23.40"}]}`))
	}))
	defer server.Close()

	svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
	ch := &ent.Channel{
		Type:    channel.TypeDeepseek,
		BaseURL: server.URL + "/v1",
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{},
		},
	}

	rules := defaultChannelKeyHealthCheckRules(ch.Type)
	require.Len(t, rules, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL, rules[0].HTTP.URLMode)

	rules[0].HTTP.URL = server.URL + "/user/balance"
	result := svc.runChannelKeyHealthCheckRule(context.Background(), ch, testKey, rules[0])

	require.True(t, result.Success, result.Reason)
	require.Equal(t, "/user/balance", gotPath)
	require.Equal(t, "Bearer "+testKey, gotAuthorization)
	require.Equal(t, "23.40", result.Balance)
	require.Equal(t, "CNY", result.Currency)
}

func TestRunHTTPChannelKeyHealthCheck_StatusFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
	ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

	result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-status-fail", objects.ChannelKeyHealthCheckHTTPRule{
		Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
		URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
		Path:             "/user/balance",
		ExpectedStatuses: []int{http.StatusOK},
	})

	require.False(t, result.Success)
	require.Contains(t, result.Reason, "unexpected status 401")
	require.NotContains(t, result.Reason, "sk-status-fail")
}

func TestRunHTTPChannelKeyHealthCheck_PassWhen(t *testing.T) {
	t.Run("true expression passes", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Provider", "deepseek")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10.50"}]}`))
		}))
		defer server.Close()

		svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
		ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

		result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-pass-when-true", objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/user/balance",
			ExpectedStatuses: []int{http.StatusOK},
			PassWhen:         `status == 200 && ok && headers["x-provider"] == "deepseek" && json.is_available == true`,
		})

		require.True(t, result.Success, result.Reason)
		require.Equal(t, "10.50", result.Balance)
		require.Equal(t, "CNY", result.Currency)
		require.NotContains(t, result.Reason, "sk-pass-when-true")
	})

	t.Run("false expression fails safely", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10.50"}]}`))
		}))
		defer server.Close()

		svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
		ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

		result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-pass-when-false", objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/user/balance",
			ExpectedStatuses: []int{http.StatusOK},
			PassWhen:         "status == 200 && json.is_available == false",
		})

		require.False(t, result.Success)
		require.Equal(t, "pass condition was not satisfied", result.Reason)
		require.NotContains(t, result.Reason, "sk-pass-when-false")
	})

	t.Run("invalid expression fails safely", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_available":true}`))
		}))
		defer server.Close()

		svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
		ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

		result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-pass-when-invalid", objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/user/balance",
			ExpectedStatuses: []int{http.StatusOK},
			PassWhen:         "status ==",
		})

		require.False(t, result.Success)
		require.Equal(t, "invalid pass condition", result.Reason)
		require.NotContains(t, result.Reason, "sk-pass-when-invalid")
	})

	t.Run("non bool expression fails at compile time", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_available":true}`))
		}))
		defer server.Close()

		svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
		ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

		result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-pass-when-non-bool", objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/user/balance",
			ExpectedStatuses: []int{http.StatusOK},
			PassWhen:         "status + 1",
		})

		require.False(t, result.Success)
		require.Equal(t, "invalid pass condition", result.Reason)
		require.NotContains(t, result.Reason, "sk-pass-when-non-bool")
	})

	t.Run("DeepSeek balance aliases can pass", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10.50"}]}`))
		}))
		defer server.Close()

		svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
		ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

		result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-pass-when-balance", objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/user/balance",
			ExpectedStatuses: []int{http.StatusOK},
			PassWhen:         `available == true && currency == "CNY" && balance > 0`,
		})

		require.True(t, result.Success, result.Reason)
		require.Equal(t, "10.50", result.Balance)
		require.Equal(t, "CNY", result.Currency)
		require.NotContains(t, result.Reason, "sk-pass-when-balance")
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"10.50"}]}`))
	}))
	defer server.Close()

	svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
	ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

	result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-pass-when", objects.ChannelKeyHealthCheckHTTPRule{
		Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
		URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
		Path:             "/user/balance",
		ExpectedStatuses: []int{http.StatusOK},
		PassWhen:         "status == 200 && json.is_available == true",
	})

	require.False(t, result.Success)
	require.Equal(t, "pass condition was not satisfied", result.Reason)
	require.NotContains(t, result.Reason, "sk-pass-when")
}

func TestAppendChannelKeyHealthCheckHistoryCapsNewestFirst(t *testing.T) {
	keyID := objects.ChannelAPIKeyFingerprint("history-key")
	now := time.Date(2026, 5, 24, 4, 30, 0, 0, time.UTC)

	history := make([]objects.ChannelKeyHealthCheckHistoryEntry, 0, channelKeyHealthCheckHistoryLimit+5)
	for i := range channelKeyHealthCheckHistoryLimit + 5 {
		history = append(history, objects.ChannelKeyHealthCheckHistoryEntry{
			ID:        keyID + ":old:" + string(rune('a'+i)),
			CheckedAt: now.Add(-time.Duration(i+1) * time.Minute),
			Success:   true,
			Trigger:   objects.ChannelKeyHealthCheckTriggerScheduled,
		})
	}

	got := appendChannelKeyHealthCheckHistory(history, keyID, ChannelKeyHealthCheckResult{
		Success:  false,
		Reason:   "manual failure",
		Balance:  "10.50",
		Currency: "CNY",
		Rule:     "DeepSeek balance",
	}, now, objects.ChannelKeyHealthCheckTriggerManual)

	require.Len(t, got, channelKeyHealthCheckHistoryLimit)
	require.Contains(t, got[0].ID, keyID)
	require.Equal(t, now, got[0].CheckedAt)
	require.False(t, got[0].Success)
	require.Equal(t, "manual failure", got[0].Reason)
	require.Equal(t, "10.50", got[0].Balance)
	require.Equal(t, "CNY", got[0].Currency)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerManual, got[0].Trigger)
	require.Equal(t, "DeepSeek balance", got[0].Rule)
}

func TestSelectedChannelKeyHealthCheckTargetsExcludeArchivedAndDefaultDisabled(t *testing.T) {
	disabledAt := time.Date(2026, 5, 24, 4, 0, 0, 0, time.UTC)
	ch := &ent.Channel{
		Credentials: objects.ChannelCredentials{APIKeys: []string{"active-key", "disabled-key", "archived-key"}},
		DisabledAPIKeys: []objects.DisabledAPIKey{{
			Key:        "disabled-key",
			DisabledAt: disabledAt,
			Reason:     "manual",
		}},
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				ArchivedKeys: []objects.ChannelArchivedAPIKey{{
					ID:        objects.ChannelAPIKeyFingerprint("archived-key"),
					MaskedKey: objects.MaskChannelAPIKey("archived-key"),
				}},
			},
		},
	}

	require.Equal(t, []string{"active-key"}, selectedChannelKeyHealthCheckTargets(ch, nil))
	require.Equal(t, []string{"disabled-key"}, selectedChannelKeyHealthCheckTargets(ch, []string{
		objects.ChannelAPIKeyFingerprint("disabled-key"),
		objects.ChannelAPIKeyFingerprint("archived-key"),
		objects.ChannelAPIKeyFingerprint("disabled-key"),
	}))
}

func TestChannelService_RunManualHealthCheckPersistsBalanceAndHistory(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer manual-key", r.Header.Get("Authorization"))
		require.Equal(t, "/user/balance", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"10.50"}]}`))
	}))
	defer server.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Manual Health Check").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"manual-key", "other-key"}}).
		SetSupportedModels([]string{"deepseek-chat"}).
		SetDefaultTestModel("deepseek-chat").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Rules: []objects.ChannelKeyHealthCheckRule{{
					ID:   "deepseek-balance",
					Name: "DeepSeek balance",
					Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
					HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
						Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
						URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
						Path:             "/user/balance",
						ExpectedStatuses: []int{http.StatusOK},
						PassWhen:         "json.is_available == true",
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	manualID := objects.ChannelAPIKeyFingerprint("manual-key")
	items, err := svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, []string{manualID})
	require.NoError(t, err)

	var got *ChannelAPIKeyInventoryItem
	for _, item := range items {
		if item.ID == manualID {
			got = item
			break
		}
	}
	require.NotNil(t, got)
	require.NotNil(t, got.Success)
	require.True(t, *got.Success)
	require.Equal(t, "10.50", got.Balance)
	require.Equal(t, "CNY", got.Currency)
	require.NotNil(t, got.Available)
	require.True(t, *got.Available)
	require.Len(t, got.History, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerManual, got.History[0].Trigger)
	require.Equal(t, "DeepSeek balance", got.History[0].Rule)
	require.Equal(t, "10.50", got.History[0].Balance)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.Equal(t, manualID, updated.Settings.KeyHealthCheck.KeyMetadata[0].ID)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata[0].History, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckTriggerManual, updated.Settings.KeyHealthCheck.KeyMetadata[0].History[0].Trigger)
}

func TestChannelService_HealthCheckFailureActionPreservesLastKeyOnDelete(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Health Check Preserve").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"only-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:          true,
				FailureThreshold: 1,
				FailureAction:    objects.ChannelKeyHealthCheckFailureActionDelete,
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID:           objects.ChannelAPIKeyFingerprint("only-key"),
					FailureCount: 1,
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.applyChannelKeyHealthCheckFailureActions(ctx, ch.ID, ch.Settings.KeyHealthCheck, []string{"only-key"}, []ChannelKeyHealthCheckResult{{
		Success: false,
		Reason:  "bad key",
	}})
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"only-key"}, updated.Credentials.APIKeys)
}

func TestChannelService_HealthCheckFailureActionPreservesLastKeyOnArchive(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Health Check Preserve Archive").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"only-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:          true,
				FailureThreshold: 1,
				FailureAction:    objects.ChannelKeyHealthCheckFailureActionArchive,
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID:           objects.ChannelAPIKeyFingerprint("only-key"),
					FailureCount: 1,
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.applyChannelKeyHealthCheckFailureActions(ctx, ch.ID, ch.Settings.KeyHealthCheck, []string{"only-key"}, []ChannelKeyHealthCheckResult{{
		Success: false,
		Reason:  "bad key",
	}})
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"only-key"}, updated.Credentials.APIKeys)
	require.Empty(t, updated.Settings.KeyHealthCheck.ArchivedKeys)
}

func TestChannelService_RunDueHealthCheckUpdatesMetadataAndDisablesFailedKey(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "bad-key") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"2"}]}`))
	}))
	defer server.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Health Check Disable").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"good-key", "bad-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:          true,
				IntervalMinutes:  60,
				FailureThreshold: 1,
				FailureAction:    objects.ChannelKeyHealthCheckFailureActionDisable,
				Rules: []objects.ChannelKeyHealthCheckRule{{
					ID:   "balance",
					Name: "Balance",
					Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
					HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
						Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
						URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
						Path:             "/user/balance",
						ExpectedStatuses: []int{http.StatusOK},
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.RunDueChannelKeyHealthChecks(ctx, time.Now())
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 2)
	require.Len(t, updated.DisabledAPIKeys, 1)
	require.Equal(t, "bad-key", updated.DisabledAPIKeys[0].Key)
	require.Equal(t, channelKeyHealthCheckFailureCode, updated.DisabledAPIKeys[0].ErrorCode)
	require.Equal(t, "unexpected status 401", updated.DisabledAPIKeys[0].Reason)
}

func TestChannelService_RunDueHealthCheckKeepsDisabledMetadataStatus(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":true}`))
	}))
	defer server.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	disabledAt := time.Now().Add(-time.Hour)
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Health Check Include Disabled").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"active-key", "disabled-key"}}).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{{
			Key:        "disabled-key",
			DisabledAt: disabledAt,
			Reason:     "manual",
		}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:          true,
				IntervalMinutes:  60,
				FailureThreshold: 1,
				FailureAction:    objects.ChannelKeyHealthCheckFailureActionReportOnly,
				IncludeDisabled:  true,
				Rules: []objects.ChannelKeyHealthCheckRule{{
					ID:   "balance",
					Name: "Balance",
					Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
					HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
						Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
						URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
						Path:             "/user/balance",
						ExpectedStatuses: []int{http.StatusOK},
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	err = svc.RunDueChannelKeyHealthChecks(ctx, time.Now())
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	metadataByID := lo.SliceToMap(updated.Settings.KeyHealthCheck.KeyMetadata, func(item objects.ChannelKeyMetadata) (string, objects.ChannelKeyMetadata) {
		return item.ID, item
	})
	disabledMeta := metadataByID[objects.ChannelAPIKeyFingerprint("disabled-key")]
	require.Equal(t, objects.ChannelKeyStatusDisabled, disabledMeta.Status)
}
