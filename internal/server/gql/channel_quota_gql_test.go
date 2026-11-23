package gql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestChannelQuotaGraphQL_BatchReadZeroExternalCallsAndRefreshPermissionGate(t *testing.T) {
	t.Setenv("AXONHUB_PROBE_SECRET_KEY", "gql-test-probe-secret-key-2026")

	var upstreamHits atomic.Int32
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamHits.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"44.20"}]}`)),
			Request:    r,
		}, nil
	})

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	seedCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	sysSvc := biz.NewSystemService(biz.SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	hc := httpclient.NewHttpClientWithClient(&http.Client{Transport: rt})
	chSvc := biz.NewChannelService(biz.ChannelServiceParams{
		Ent:           client,
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		SystemService: sysSvc,
		HttpClient:    hc,
	})
	defer chSvc.Stop()

	pqSvc := biz.NewProviderQuotaService(biz.ProviderQuotaServiceParams{
		Ent:           client,
		SystemService: sysSvc,
		HttpClient:    hc,
	})

	adminUser := &ent.User{ID: 1, IsOwner: true, Scopes: []string{string(scopes.ScopeReadChannels), string(scopes.ScopeWriteChannels)}}
	adminCtx := contexts.WithUser(seedCtx, adminUser)

	// Create 2 channels: DeepSeek + Exa with encrypted ProbeAPIKey.
	const exaSecret = "exa-top-secret-admin-key"
	dsCh, err := chSvc.CreateChannel(adminCtx, ent.CreateChannelInput{
		Name:             "deepseek-gql",
		Type:             channel.TypeDeepseek,
		BaseURL:          lo.ToPtr("https://api.deepseek.com/v1"),
		SupportedModels:  []string{"deepseek-chat"},
		DefaultTestModel: "deepseek-chat",
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"sk-ds-gql-1"}},
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: true,
				Preset:  objects.ChannelBalanceProbePresetDeepSeek,
			},
		},
	})
	require.NoError(t, err)

	now := time.Now().UTC().Truncate(time.Second)
	exaCh, err := chSvc.CreateChannel(adminCtx, ent.CreateChannelInput{
		Name:             "exa-gql",
		Type:             channel.TypeOpenai,
		BaseURL:          lo.ToPtr("https://api.exa.ai"),
		SupportedModels:  []string{"exa"},
		DefaultTestModel: "exa",
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"exa-inf-1", "exa-inf-2"}},
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:       false,
				Preset:        objects.ChannelBalanceProbePresetExa,
				SharedWallet:  true,
				ProbeAPIKeyOp: objects.ChannelSecretWriteOpSet,
				ProbeAPIKey:   exaSecret,
			},
		},
	})
	require.NoError(t, err)
	// Persist server-managed SharedBalanceSnapshot as if a probe completed earlier.
	exaSettings := *exaCh.Settings
	exaSettings.KeyHealthCheck = &objects.ChannelKeyHealthCheck{
		SharedBalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
			Success:        true,
			CheckedAt:      now,
			Available:      lo.ToPtr(true),
			Remaining:      lo.ToPtr(75.0),
			WalletScope:    "shared_wallet",
			PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 75, Kind: objects.BalanceAmountKindRemaining},
		},
	}
	exaCh, err = client.Channel.UpdateOneID(exaCh.ID).SetSettings(&exaSettings).Save(seedCtx)
	require.NoError(t, err)

	srv := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: &Resolver{
			client:               client,
			channelService:       chSvc,
			providerQuotaService: pqSvc,
		},
	}))

	execGQL := func(ctx context.Context, query string, vars map[string]any) (int, []byte) {
		body, marshalErr := json.Marshal(map[string]any{"query": query, "variables": vars})
		require.NoError(t, marshalErr)
		req := httptest.NewRequest(http.MethodPost, "/admin/graphql", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(biz.WithChannelQuotaOverviewBatch(ctx, biz.NewChannelQuotaOverviewBatch()))
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}

	readOnlyUser := &ent.User{ID: 2, IsOwner: false, Scopes: []string{string(scopes.ScopeReadChannels)}}
	readOnlyCtx := contexts.WithUser(ent.NewContext(context.Background(), client), readOnlyUser)
	readOnlyCtx, err = authz.WithPrincipal(readOnlyCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)

	// 1. Batch list query with quotaOverview & hasProbeAPIKey:
	// - 0 external HTTP calls
	// - Exa sharedWallet=true, walletRemaining=75 (not multiplied by 2), hasProbeAPIKey=true
	// - Zero plaintext exaSecret anywhere in GraphQL response!
	listQuery := `query {
		channels(first: 10) {
			edges {
				node {
					id
					name
					settings {
						balanceProbe {
							preset
							sharedWallet
							hasProbeAPIKey
						}
					}
					quotaOverview {
						state
						sharedWallet
						walletRemaining
						counts { total active freshKnown exhausted unknown }
					}
				}
			}
		}
	}`
	code, raw := execGQL(readOnlyCtx, listQuery, nil)
	require.Equal(t, http.StatusOK, code)
	require.NotContains(t, string(raw), exaSecret, "GraphQL response MUST NEVER leak plaintext ProbeAPIKey")
	require.Equal(t, int32(0), upstreamHits.Load(), "list query with quotaOverview must make 0 external HTTP calls")

	var listResp struct {
		Data struct {
			Channels struct {
				Edges []struct {
					Node struct {
						Name     string `json:"name"`
						Settings *struct {
							BalanceProbe *struct {
								Preset         string `json:"preset"`
								SharedWallet   bool   `json:"sharedWallet"`
								HasProbeAPIKey bool   `json:"hasProbeAPIKey"`
							} `json:"balanceProbe"`
						} `json:"settings"`
						QuotaOverview *struct {
							State           string   `json:"state"`
							SharedWallet    bool     `json:"sharedWallet"`
							WalletRemaining *float64 `json:"walletRemaining"`
							Counts          struct {
								Total int `json:"total"`
							} `json:"counts"`
						} `json:"quotaOverview"`
					} `json:"node"`
				} `json:"edges"`
			} `json:"channels"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(raw, &listResp))
	require.Empty(t, listResp.Errors)
	require.Len(t, listResp.Data.Channels.Edges, 2)

	foundExa := false
	for _, edge := range listResp.Data.Channels.Edges {
		if edge.Node.Name == exaCh.Name {
			foundExa = true
			require.True(t, edge.Node.Settings.BalanceProbe.HasProbeAPIKey)
			require.True(t, edge.Node.QuotaOverview.SharedWallet)
			require.NotNil(t, edge.Node.QuotaOverview.WalletRemaining)
			require.InDelta(t, 75.0, *edge.Node.QuotaOverview.WalletRemaining, 1e-6)
		}
	}
	require.True(t, foundExa)

	// 2. RefreshChannelQuota mutation MUST reject read-only user lacking write_channels!
	refreshMutation := `mutation($id: ID!) {
		refreshChannelQuota(channelID: $id) {
			status
			updatedKeys
			quotaOverview {
				walletRemaining
			}
		}
	}`
	guidStr := fmt.Sprintf("gid://axonhub/Channel/%d", dsCh.ID)
	_, rawDenied := execGQL(readOnlyCtx, refreshMutation, map[string]any{"id": guidStr})
	require.Contains(t, string(rawDenied), "permission denied")
	require.Equal(t, int32(0), upstreamHits.Load())

	// 3. RefreshChannelQuota with write_channels succeeds and returns updated balance!
	writerCtx := contexts.WithUser(ent.NewContext(context.Background(), client), adminUser)
	writerCtx, err = authz.WithPrincipal(writerCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)
	code, rawOK := execGQL(writerCtx, refreshMutation, map[string]any{"id": guidStr})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, int32(1), upstreamHits.Load())
	var mutResp struct {
		Data struct {
			RefreshChannelQuota struct {
				Status        string `json:"status"`
				UpdatedKeys   int    `json:"updatedKeys"`
				QuotaOverview *struct {
					WalletRemaining *float64 `json:"walletRemaining"`
				} `json:"quotaOverview"`
			} `json:"refreshChannelQuota"`
		} `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rawOK, &mutResp))
	require.Empty(t, mutResp.Errors)
	require.Equal(t, "updated", mutResp.Data.RefreshChannelQuota.Status)
	require.Equal(t, 1, mutResp.Data.RefreshChannelQuota.UpdatedKeys)
	require.NotNil(t, mutResp.Data.RefreshChannelQuota.QuotaOverview.WalletRemaining)
	require.InDelta(t, 44.20, *mutResp.Data.RefreshChannelQuota.QuotaOverview.WalletRemaining, 1e-6)

	// 4. GraphQL tracer MUST omit raw payload & variables for mutations (including block
	// strings & variable defaults) AND recursively redact sensitive variable keys!
	require.True(t, shouldOmitGraphQLDebugPayload(&graphql.OperationContext{
		RawQuery: `mutation($k: String = """exa-top-secret-admin-key""") { updateChannel(input: { settings: { balanceProbe: { probeAPIKey: $k } } }) { id } }`,
	}))
	redactedVars := redactGraphQLValue(map[string]any{
		"input": map[string]any{
			"settings": map[string]any{
				"balanceProbe": map[string]any{
					"probeAPIKey": exaSecret,
				},
			},
		},
	})
	varsBytes, _ := json.Marshal(redactedVars)
	require.NotContains(t, string(varsBytes), exaSecret)
	require.Contains(t, string(varsBytes), "[REDACTED]")
}
