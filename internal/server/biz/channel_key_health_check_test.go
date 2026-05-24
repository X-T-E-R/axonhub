package biz

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/llm/httpclient"
)

type channelKeyHealthCheckRoundTripper func(req *http.Request) (*http.Response, error)

func (f channelKeyHealthCheckRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

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
	transport := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		gotAuthorization = r.Header.Get("Authorization")

		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"23.40"}]}`))),
			Request:    r,
		}, nil
	})

	svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(&http.Client{Transport: transport})}
	ch := &ent.Channel{
		Type:    channel.TypeDeepseek,
		BaseURL: "https://api.deepseek.com/v1",
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{},
		},
	}

	rules := defaultChannelKeyHealthCheckRules(ch.Type)
	require.Len(t, rules, 1)
	require.Equal(t, objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL, rules[0].HTTP.URLMode)

	rules[0].HTTP.URL = "https://api.deepseek.com/user/balance"
	result := svc.runChannelKeyHealthCheckRule(context.Background(), ch, testKey, rules[0])

	require.True(t, result.Success, result.Reason)
	require.Equal(t, "/user/balance", gotPath)
	require.Equal(t, "Bearer "+testKey, gotAuthorization)
	require.Equal(t, "23.40", result.Balance)
	require.Equal(t, "CNY", result.Currency)
}

func TestBuildChannelKeyHealthCheckHTTPURL_EgressValidation(t *testing.T) {
	t.Run("provider base relative path allowed", func(t *testing.T) {
		got, err := buildChannelKeyHealthCheckHTTPURL(context.Background(), "http://127.0.0.1:8080/v1", objects.ChannelKeyHealthCheckHTTPRule{
			URLMode: objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:    "/user/balance",
		})
		require.NoError(t, err)
		require.Equal(t, "http://127.0.0.1:8080/v1/user/balance", got)
	})

	t.Run("public absolute address allowed", func(t *testing.T) {
		got, err := buildChannelKeyHealthCheckHTTPURL(context.Background(), "https://93.184.216.34/v1", objects.ChannelKeyHealthCheckHTTPRule{
			URLMode: objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:     "https://93.184.216.34/user/balance",
		})
		require.NoError(t, err)
		require.Equal(t, "https://93.184.216.34/user/balance", got)
	})

	t.Run("cross origin absolute address blocked", func(t *testing.T) {
		_, err := buildChannelKeyHealthCheckHTTPURL(context.Background(), "https://api.deepseek.com/v1", objects.ChannelKeyHealthCheckHTTPRule{
			URLMode: objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:     "https://93.184.216.34/user/balance",
		})
		require.ErrorContains(t, err, "must match provider base origin")
	})

	blocked := []string{
		"http://127.0.0.1/user/balance",
		"http://10.0.0.1/user/balance",
		"http://169.254.169.254/latest/meta-data",
		"http://metadata.google.internal/computeMetadata/v1",
	}
	for _, rawURL := range blocked {
		t.Run(rawURL, func(t *testing.T) {
			baseURL := rawURL
			if idx := strings.Index(rawURL, "/latest/"); idx > 0 {
				baseURL = rawURL[:idx]
			} else if idx := strings.Index(rawURL, "/computeMetadata/"); idx > 0 {
				baseURL = rawURL[:idx]
			} else if idx := strings.LastIndex(rawURL, "/"); idx > len("http://") {
				baseURL = rawURL[:idx]
			}
			_, err := buildChannelKeyHealthCheckHTTPURL(context.Background(), baseURL, objects.ChannelKeyHealthCheckHTTPRule{
				URLMode: objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
				URL:     rawURL,
			})
			require.ErrorContains(t, err, "not allowed")
		})
	}
}

func TestRunHTTPChannelKeyHealthCheck_BlocksRedirects(t *testing.T) {
	var finalHits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			finalHits.Add(1)
			_, _ = w.Write([]byte(`{"is_available":true}`))
			return
		}

		http.Redirect(w, r, "/final", http.StatusFound)
	}))
	defer server.Close()

	svc := &ChannelService{httpClient: httpclient.NewHttpClientWithClient(server.Client())}
	ch := &ent.Channel{BaseURL: server.URL, Settings: &objects.ChannelSettings{}}

	result := svc.runHTTPChannelKeyHealthCheck(context.Background(), ch, "sk-redirect", objects.ChannelKeyHealthCheckHTTPRule{
		Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
		URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
		Path:             "/redirect",
		ExpectedStatuses: []int{http.StatusOK},
	})

	require.False(t, result.Success)
	require.Equal(t, http.StatusFound, result.StatusCode)
	require.Equal(t, int32(0), finalHits.Load())
	require.NotContains(t, result.Reason, "sk-redirect")
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
	require.Equal(t, http.StatusUnauthorized, result.StatusCode)
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
	}, now, objects.ChannelKeyHealthCheckTriggerManual, channelKeyHealthCheckHistoryLimit)

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

func TestChannelKeyHealthCheckPolicy_StatusBackoffReportOnly(t *testing.T) {
	now := time.Date(2026, 5, 24, 5, 0, 0, 0, time.UTC)
	health := &objects.ChannelKeyHealthCheck{
		KeyMetadata: []objects.ChannelKeyMetadata{{
			ID:             objects.ChannelAPIKeyFingerprint("rate-limit-key"),
			FailureCount:   1,
			BackoffAttempt: 1,
		}},
		Policies: []objects.ChannelKeyHealthCheckPolicy{{
			ID:   "rate-limit",
			Name: "Rate limit",
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				MinFailureCount: lo.ToPtr(2),
				StatusCodes:     []int{http.StatusTooManyRequests},
			},
			Actions: []objects.ChannelKeyHealthCheckPolicyAction{
				{Type: objects.ChannelKeyHealthCheckPolicyActionReportOnly},
				{Type: objects.ChannelKeyHealthCheckPolicyActionBackoff, Backoff: &objects.ChannelKeyHealthCheckBackoff{
					Mode:               objects.ChannelKeyHealthCheckBackoffModeExponential,
					IntervalMinutes:    5,
					MaxIntervalMinutes: 20,
					Multiplier:         2,
				}},
			},
		}},
	}

	results := applyChannelKeyHealthCheckPoliciesToResults(health, []string{"rate-limit-key"}, []ChannelKeyHealthCheckResult{{
		Success:    false,
		Reason:     "unexpected status 429",
		StatusCode: http.StatusTooManyRequests,
	}}, now, objects.ChannelKeyHealthCheckTriggerScheduled, false)

	require.Len(t, results, 1)
	require.Equal(t, "Rate limit", results[0].MatchedPolicy)
	require.Equal(t, "report_only,backoff", results[0].Action)
	require.Equal(t, 2, results[0].BackoffAttempt)
	require.NotNil(t, results[0].NextCheckAt)
	require.Equal(t, now.Add(10*time.Minute), *results[0].NextCheckAt)

	metadata := upsertChannelKeyHealthCheckMetadata(nil, "rate-limit-key", results[0], now, objects.ChannelKeyHealthCheckTriggerScheduled, channelKeyHealthCheckHistoryLimit)
	require.Len(t, metadata, 1)
	require.Equal(t, http.StatusTooManyRequests, metadata[0].StatusCode)
	require.Equal(t, "Rate limit", metadata[0].MatchedPolicy)
	require.Equal(t, "report_only,backoff", metadata[0].Action)
	require.Equal(t, 2, metadata[0].BackoffAttempt)
	require.NotNil(t, metadata[0].NextCheckAt)
	require.Equal(t, http.StatusTooManyRequests, metadata[0].History[0].StatusCode)
	require.Equal(t, "Rate limit", metadata[0].History[0].MatchedPolicy)
}

func TestChannelKeyHealthCheckPolicy_BalanceAvailabilityAndExprConditions(t *testing.T) {
	health := &objects.ChannelKeyHealthCheck{
		Policies: []objects.ChannelKeyHealthCheckPolicy{{
			ID:   "exhausted",
			Name: "Exhausted",
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				Available:      lo.ToPtr(false),
				BalanceLTE:     lo.ToPtr(0.0),
				ReasonContains: "unavailable",
				Expr:           `success == false && available == false && currency == "CNY" && trigger == "manual"`,
			},
			Actions: []objects.ChannelKeyHealthCheckPolicyAction{{Type: objects.ChannelKeyHealthCheckPolicyActionArchiveKey}},
		}},
	}

	available := false
	results := applyChannelKeyHealthCheckPoliciesToResults(health, []string{"empty-key"}, []ChannelKeyHealthCheckResult{{
		Success:   false,
		Reason:    "provider unavailable",
		Balance:   "0",
		Currency:  "CNY",
		Available: &available,
	}}, time.Now(), objects.ChannelKeyHealthCheckTriggerManual, false)

	require.Equal(t, "Exhausted", results[0].MatchedPolicy)
	require.Equal(t, string(objects.ChannelKeyHealthCheckPolicyActionArchiveKey), results[0].Action)
}

func TestChannelKeyHealthCheckPolicy_EmptyConditionsIgnored(t *testing.T) {
	health := &objects.ChannelKeyHealthCheck{
		Policies: []objects.ChannelKeyHealthCheckPolicy{{
			ID:      "empty",
			Name:    "Empty",
			Actions: []objects.ChannelKeyHealthCheckPolicyAction{{Type: objects.ChannelKeyHealthCheckPolicyActionDisableKey}},
		}},
	}

	results := applyChannelKeyHealthCheckPoliciesToResults(health, []string{"key"}, []ChannelKeyHealthCheckResult{{
		Success: false,
		Reason:  "failed",
	}}, time.Now(), objects.ChannelKeyHealthCheckTriggerScheduled, false)

	require.Empty(t, results[0].MatchedPolicy)
	require.Empty(t, results[0].Action)
}

func TestValidateChannelKeyHealthCheckPolicy(t *testing.T) {
	valid := &objects.ChannelKeyHealthCheck{
		HistoryLimit: 100,
		Policies: []objects.ChannelKeyHealthCheckPolicy{{
			ID:   "rate-limit",
			Name: "Rate limit",
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				MinFailureCount: lo.ToPtr(1),
				StatusCodes:     []int{http.StatusTooManyRequests},
				Expr:            `status == 429`,
			},
			Actions: []objects.ChannelKeyHealthCheckPolicyAction{{
				Type: objects.ChannelKeyHealthCheckPolicyActionBackoff,
				Backoff: &objects.ChannelKeyHealthCheckBackoff{
					Mode:               objects.ChannelKeyHealthCheckBackoffModeExponential,
					IntervalMinutes:    5,
					MaxIntervalMinutes: 20,
					Multiplier:         2,
				},
			}},
		}},
	}
	require.NoError(t, ValidateChannelKeyHealthCheck(valid))

	invalidHistory := *valid
	invalidHistory.HistoryLimit = 101
	require.ErrorContains(t, ValidateChannelKeyHealthCheck(&invalidHistory), "history limit")

	invalidPolicy := *valid
	invalidPolicy.Policies = []objects.ChannelKeyHealthCheckPolicy{{
		ID:      "bad",
		Name:    "Bad",
		Actions: []objects.ChannelKeyHealthCheckPolicyAction{{Type: objects.ChannelKeyHealthCheckPolicyActionBackoff}},
	}}
	require.ErrorContains(t, ValidateChannelKeyHealthCheck(&invalidPolicy), "backoff is required")

	invalidStatus := *valid
	invalidStatus.Policies = []objects.ChannelKeyHealthCheckPolicy{{
		ID:   "bad-status",
		Name: "Bad status",
		Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
			StatusCodes: []int{99},
		},
		Actions: []objects.ChannelKeyHealthCheckPolicyAction{{Type: objects.ChannelKeyHealthCheckPolicyActionReportOnly}},
	}}
	require.ErrorContains(t, ValidateChannelKeyHealthCheck(&invalidStatus), "outside HTTP status range")
}

func TestValidateChannelKeyHealthCheckHTTPRuleEgressAndHeaders(t *testing.T) {
	valid := &objects.ChannelKeyHealthCheck{
		Rules: []objects.ChannelKeyHealthCheckRule{{
			ID:   "public-absolute",
			Name: "Public absolute",
			Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
			HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
				Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
				URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
				URL:              "https://93.184.216.34/user/balance",
				ExpectedStatuses: []int{http.StatusOK},
				KeyInjection: &objects.ChannelKeyHealthCheckKeyInjection{
					Location:   objects.ChannelKeyHealthCheckKeyInjectionHeader,
					HeaderName: "X-API-Key",
				},
			},
		}},
	}
	require.NoError(t, ValidateChannelKeyHealthCheck(valid))
	require.NoError(t, validateChannelKeyHealthCheckURLsForBaseURL("https://93.184.216.34/v1", valid))

	require.ErrorContains(t, validateChannelKeyHealthCheckURLsForBaseURL("https://api.deepseek.com/v1", valid), "must match provider base origin")

	loopback := *valid
	loopback.Rules = []objects.ChannelKeyHealthCheckRule{{
		ID:   "loopback",
		Name: "Loopback",
		Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
		HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
			URLMode: objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:     "http://127.0.0.1/user/balance",
		},
	}}
	require.ErrorContains(t, ValidateChannelKeyHealthCheck(&loopback), "not allowed")

	invalidHeader := *valid
	invalidHeader.Rules = []objects.ChannelKeyHealthCheckRule{{
		ID:   "bad-header",
		Name: "Bad Header",
		Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
		HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
			URLMode: objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:    "/user/balance",
			KeyInjection: &objects.ChannelKeyHealthCheckKeyInjection{
				Location:   objects.ChannelKeyHealthCheckKeyInjectionHeader,
				HeaderName: "Bad Header",
			},
		},
	}}
	require.ErrorContains(t, ValidateChannelKeyHealthCheck(&invalidHeader), "header name is invalid")
}

func TestChannelKeyHealthCheckBackoffDueScheduling(t *testing.T) {
	now := time.Date(2026, 5, 24, 6, 0, 0, 0, time.UTC)
	nextCheck := now.Add(15 * time.Minute)
	ch := &ent.Channel{
		Status: channel.StatusEnabled,
		Credentials: objects.ChannelCredentials{
			APIKeys: []string{"backoff-key"},
		},
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Enabled:         true,
				IntervalMinutes: 1,
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID:            objects.ChannelAPIKeyFingerprint("backoff-key"),
					LastCheckedAt: lo.ToPtr(now.Add(-time.Hour)),
					NextCheckAt:   &nextCheck,
				}},
			},
		},
	}

	require.False(t, channelKeyHealthCheckDue(ch, now))
	require.Empty(t, filterChannelKeyHealthCheckDueKeys(ch, []string{"backoff-key"}, now))
	require.True(t, channelKeyHealthCheckDue(ch, nextCheck))
	require.Equal(t, []string{"backoff-key"}, filterChannelKeyHealthCheckDueKeys(ch, []string{"backoff-key"}, nextCheck))
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

func TestChannelService_RunManualHealthCheckRequiresWriteBeforeOutbound(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"is_available":true}`))
	}))
	defer server.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Manual Health Check Unauthorized").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"manual-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
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
		Save(setupCtx)
	require.NoError(t, err)

	readOnlyUser := &ent.User{
		ID:     12345,
		Scopes: []string{string(scopes.ScopeReadChannels)},
	}
	runCtx := ent.NewContext(context.Background(), client)
	runCtx = contexts.WithUser(runCtx, readOnlyUser)
	runCtx = authz.NewUserContext(runCtx, readOnlyUser.ID)

	_, err = svc.RunChannelAPIKeyHealthCheck(runCtx, ch.ID, nil)
	require.ErrorContains(t, err, string(scopes.ScopeWriteChannels))
	require.Equal(t, int32(0), hits.Load())
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
	}}, false)
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
	}}, false)
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"only-key"}, updated.Credentials.APIKeys)
	require.Empty(t, updated.Settings.KeyHealthCheck.ArchivedKeys)
}

func TestChannelService_PolicyDisableKeyAndArchivePreserveLastKey(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	available := false

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Policy Disable Archive").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"first-key", "last-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Policies: []objects.ChannelKeyHealthCheckPolicy{{
					ID:   "unavailable",
					Name: "Unavailable",
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						Available: lo.ToPtr(false),
					},
					Actions: []objects.ChannelKeyHealthCheckPolicyAction{
						{Type: objects.ChannelKeyHealthCheckPolicyActionDisableKey},
						{Type: objects.ChannelKeyHealthCheckPolicyActionArchiveKey},
					},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	results := applyChannelKeyHealthCheckPoliciesToResults(ch.Settings.KeyHealthCheck, []string{"first-key", "last-key"}, []ChannelKeyHealthCheckResult{
		{Success: false, Reason: "provider unavailable", Available: &available},
		{Success: false, Reason: "provider unavailable", Available: &available},
	}, time.Now(), objects.ChannelKeyHealthCheckTriggerScheduled, true)

	err = svc.applyChannelKeyHealthCheckFailureActions(ctx, ch.ID, ch.Settings.KeyHealthCheck, []string{"first-key", "last-key"}, results, true)
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.DisabledAPIKeys, 2)
	require.Len(t, updated.Settings.KeyHealthCheck.ArchivedKeys, 1)
	require.Equal(t, objects.ChannelAPIKeyFingerprint("first-key"), updated.Settings.KeyHealthCheck.ArchivedKeys[0].ID)
}

func TestChannelService_PolicyAllCheckedKeysFailedDisablesChannel(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Policy Disable Channel").
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"first-key", "second-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				Policies: []objects.ChannelKeyHealthCheckPolicy{{
					ID:   "all-failed",
					Name: "All keys failed",
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						AllCheckedKeysFailed: lo.ToPtr(true),
					},
					Actions: []objects.ChannelKeyHealthCheckPolicyAction{{Type: objects.ChannelKeyHealthCheckPolicyActionDisableChannel}},
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	results := applyChannelKeyHealthCheckPoliciesToResults(ch.Settings.KeyHealthCheck, []string{"first-key", "second-key"}, []ChannelKeyHealthCheckResult{
		{Success: false, Reason: "failed"},
		{Success: false, Reason: "failed"},
	}, time.Now(), objects.ChannelKeyHealthCheckTriggerScheduled, true)

	err = svc.applyChannelKeyHealthCheckFailureActions(ctx, ch.ID, ch.Settings.KeyHealthCheck, []string{"first-key", "second-key"}, results, true)
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, channel.StatusDisabled, updated.Status)
	require.NotNil(t, updated.ErrorMessage)
	require.Contains(t, *updated.ErrorMessage, "All keys failed")
}

func TestChannelService_RunManualHealthCheckSuccessResetsBackoff(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"is_available":true}`))
	}))
	defer server.Close()
	svc.httpClient = httpclient.NewHttpClientWithClient(server.Client())

	nextCheck := time.Now().Add(time.Hour)
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Health Check Reset Backoff").
		SetStatus(channel.StatusEnabled).
		SetBaseURL(server.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"reset-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
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
				KeyMetadata: []objects.ChannelKeyMetadata{{
					ID:             objects.ChannelAPIKeyFingerprint("reset-key"),
					FailureCount:   2,
					NextCheckAt:    &nextCheck,
					BackoffAttempt: 2,
				}},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	_, err = svc.RunChannelAPIKeyHealthCheck(ctx, ch.ID, []string{objects.ChannelAPIKeyFingerprint("reset-key")})
	require.NoError(t, err)

	updated, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Len(t, updated.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.Equal(t, 0, updated.Settings.KeyHealthCheck.KeyMetadata[0].FailureCount)
	require.Nil(t, updated.Settings.KeyHealthCheck.KeyMetadata[0].NextCheckAt)
	require.Equal(t, 0, updated.Settings.KeyHealthCheck.KeyMetadata[0].BackoffAttempt)
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
