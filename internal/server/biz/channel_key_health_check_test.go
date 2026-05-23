package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
