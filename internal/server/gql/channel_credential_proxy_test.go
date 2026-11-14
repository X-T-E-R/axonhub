package gql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

type gqlHTTPResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func execGQLWithStatus(t *testing.T, srv http.Handler, ctx context.Context, expectedStatus int, query string, variables map[string]any) gqlHTTPResponse {
	t.Helper()

	payload := map[string]any{
		"query": query,
	}
	if variables != nil {
		payload["variables"] = variables
	}
	body, err := json.Marshal(payload)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	require.Equal(t, expectedStatus, rec.Code, rec.Body.String())

	var out gqlHTTPResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

func execGQL(t *testing.T, srv http.Handler, ctx context.Context, query string, variables map[string]any) gqlHTTPResponse {
	t.Helper()
	return execGQLWithStatus(t, srv, ctx, http.StatusOK, query, variables)
}

func setupCredentialProxyGQLTest(t *testing.T, httpClient *httpclient.HttpClient) (*handler.Server, *ent.Client, context.Context, context.Context, *ent.Project) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	seedCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	hashedPassword, err := biz.HashPassword("test-password")
	require.NoError(t, err)

	ownerUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("owner-%d@example.com", time.Now().UnixNano())).
		SetPassword(hashedPassword).
		SetFirstName("Owner").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		SetScopes([]string{
			string(scopes.ScopeReadChannels),
			string(scopes.ScopeWriteChannels),
			string(scopes.ScopeReadAPIKeys),
			string(scopes.ScopeWriteAPIKeys),
		}).
		Save(seedCtx)
	require.NoError(t, err)

	readerUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("reader-%d@example.com", time.Now().UnixNano())).
		SetPassword(hashedPassword).
		SetFirstName("Reader").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		SetIsOwner(false).
		SetScopes([]string{string(scopes.ScopeReadChannels)}).
		Save(seedCtx)
	require.NoError(t, err)

	testProject, err := client.Project.Create().
		SetName(fmt.Sprintf("gql-project-%d", time.Now().UnixNano())).
		SetDescription("GQL test project").
		SetStatus(project.StatusActive).
		Save(seedCtx)
	require.NoError(t, err)

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	sysSvc := biz.NewSystemService(biz.SystemServiceParams{
		Ent:         client,
		CacheConfig: cacheConfig,
	})
	if httpClient == nil {
		httpClient = httpclient.NewHttpClient()
	}
	channelSvc := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   cacheConfig,
		Ent:           client,
		SystemService: sysSvc,
		HttpClient:    httpClient,
	})
	t.Cleanup(channelSvc.Stop)

	outboundProxySvc := biz.NewOutboundProxyService(biz.OutboundProxyServiceParams{
		Ent: client,
	})
	projectSvc := &biz.ProjectService{
		ProjectCache: xcache.NewFromConfig[xcache.Entry[ent.Project]](cacheConfig),
	}
	apiKeySvc := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    cacheConfig,
		Ent:            client,
		ProjectService: projectSvc,
		KeyPrefix:      "ah",
	})
	t.Cleanup(apiKeySvc.Stop)

	server := handler.NewDefaultServer(NewExecutableSchema(Config{
		Resolvers: &Resolver{
			client:               client,
			channelService:       channelSvc,
			outboundProxyService: outboundProxySvc,
			apiKeyService:        apiKeySvc,
			projectService:       projectSvc,
		},
	}))

	ownerCtx := contexts.WithUser(ent.NewContext(context.Background(), client), ownerUser)
	ownerCtx, err = authz.WithPrincipal(ownerCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)

	readerCtx := contexts.WithUser(ent.NewContext(context.Background(), client), readerUser)
	readerCtx, err = authz.WithPrincipal(readerCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)

	return server, client, ownerCtx, readerCtx, testProject
}

func TestGraphQL_OutboundProxy_CRUDRedactionAndPermissions(t *testing.T) {
	server, client, ownerCtx, readerCtx, _ := setupCredentialProxyGQLTest(t, nil)
	defer client.Close()

	// Schema must not expose raw proxyUrl on OutboundProxy output type.
	badRes := execGQLWithStatus(t, server, ownerCtx, http.StatusUnprocessableEntity, `query { outboundProxies { id proxyUrl } }`, nil)
	require.NotEmpty(t, badRes.Errors)

	// Read-only user cannot mutate managed proxies.
	deniedCreate := execGQL(t, server, readerCtx, `
		mutation CreateProxy($input: CreateOutboundProxyInput!) {
			createOutboundProxy(input: $input) { id }
		}
	`, map[string]any{
		"input": map[string]any{
			"name":     "denied-proxy",
			"proxyUrl": "http://user:pass@proxy.example.com:8080",
		},
	})
	require.NotEmpty(t, deniedCreate.Errors)

	// Owner creates a managed proxy with embedded credentials and query secrets.
	createRes := execGQL(t, server, ownerCtx, `
		mutation CreateProxy($input: CreateOutboundProxyInput!) {
			createOutboundProxy(input: $input) {
				id
				name
				kind
				maskedUrl
				poolName
				status
				weight
				maxConcurrent
				maxCredentials
				remark
				credentialReferenceCount
				channelReferenceCount
			}
		}
	`, map[string]any{
		"input": map[string]any{
			"name":           "east-proxy-1",
			"kind":           "static",
			"proxyUrl":       "http://secret-user:secret-pass@east.proxy.internal:8443/egress?token=super-secret-token",
			"poolName":       "pool-east",
			"status":         "enabled",
			"weight":         5,
			"maxConcurrent":  4,
			"maxCredentials": 2,
			"remark":         "initial remark",
		},
	})
	require.Empty(t, createRes.Errors)

	var createdPayload struct {
		CreateOutboundProxy struct {
			ID                       string `json:"id"`
			Name                     string `json:"name"`
			Kind                     string `json:"kind"`
			MaskedURL                string `json:"maskedUrl"`
			PoolName                 string `json:"poolName"`
			Status                   string `json:"status"`
			Weight                   int    `json:"weight"`
			MaxConcurrent            *int   `json:"maxConcurrent"`
			MaxCredentials           *int   `json:"maxCredentials"`
			Remark                   string `json:"remark"`
			CredentialReferenceCount int    `json:"credentialReferenceCount"`
			ChannelReferenceCount    int    `json:"channelReferenceCount"`
		} `json:"createOutboundProxy"`
	}
	require.NoError(t, json.Unmarshal(createRes.Data, &createdPayload))
	created := createdPayload.CreateOutboundProxy
	require.NotEmpty(t, created.ID)
	require.Equal(t, "east-proxy-1", created.Name)
	require.Equal(t, "static", created.Kind)
	require.Equal(t, "pool-east", created.PoolName)
	require.Equal(t, "enabled", created.Status)
	require.Equal(t, 5, created.Weight)
	require.Equal(t, 4, *created.MaxConcurrent)
	require.Equal(t, 2, *created.MaxCredentials)
	require.Contains(t, created.MaskedURL, "east.proxy.internal:8443")
	require.NotContains(t, created.MaskedURL, "secret-user")
	require.NotContains(t, created.MaskedURL, "secret-pass")
	require.NotContains(t, created.MaskedURL, "super-secret-token")

	// Update proxy to dynamic_template with a new secret URL and updated pool/limits.
	updateRes := execGQL(t, server, ownerCtx, `
		mutation UpdateProxy($id: ID!, $input: UpdateOutboundProxyInput!) {
			updateOutboundProxy(id: $id, input: $input) {
				id
				name
				kind
				maskedUrl
				poolName
				maxCredentials
				remark
			}
		}
	`, map[string]any{
		"id": created.ID,
		"input": map[string]any{
			"name":           "east-proxy-1-v2",
			"kind":           "dynamic_template",
			"proxyUrl":       "http://proxy-user:new-secret-pw@dyn.proxy.internal:9000/{credential_id}",
			"poolName":       "pool-west",
			"maxCredentials": 3,
			"remark":         "updated remark",
		},
	})
	require.Empty(t, updateRes.Errors)

	var updatedPayload struct {
		UpdateOutboundProxy struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Kind           string `json:"kind"`
			MaskedURL      string `json:"maskedUrl"`
			PoolName       string `json:"poolName"`
			MaxCredentials *int   `json:"maxCredentials"`
			Remark         string `json:"remark"`
		} `json:"updateOutboundProxy"`
	}
	require.NoError(t, json.Unmarshal(updateRes.Data, &updatedPayload))
	require.Equal(t, "east-proxy-1-v2", updatedPayload.UpdateOutboundProxy.Name)
	require.Equal(t, "dynamic_template", updatedPayload.UpdateOutboundProxy.Kind)
	require.Equal(t, "pool-west", updatedPayload.UpdateOutboundProxy.PoolName)
	require.Equal(t, 3, *updatedPayload.UpdateOutboundProxy.MaxCredentials)
	require.Equal(t, "http://redacted@dyn.proxy.internal:9000/redacted", updatedPayload.UpdateOutboundProxy.MaskedURL)
	require.NotContains(t, updatedPayload.UpdateOutboundProxy.MaskedURL, "new-secret-pw")

	// Read-only user can list outboundProxies & outboundProxyPools with full redaction.
	listRes := execGQL(t, server, readerCtx, `
		query ListProxiesAndPools {
			outboundProxies {
				id
				name
				kind
				maskedUrl
				poolName
				status
				maxCredentials
			}
			outboundProxyPools {
				name
				proxyCount
				enabledProxyCount
				credentialReferenceCount
			}
		}
	`, nil)
	require.Empty(t, listRes.Errors)

	var listPayload struct {
		OutboundProxies []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			MaskedURL string `json:"maskedUrl"`
			PoolName  string `json:"poolName"`
		} `json:"outboundProxies"`
		OutboundProxyPools []struct {
			Name                     string `json:"name"`
			ProxyCount               int    `json:"proxyCount"`
			EnabledProxyCount        int    `json:"enabledProxyCount"`
			CredentialReferenceCount int    `json:"credentialReferenceCount"`
		} `json:"outboundProxyPools"`
	}
	require.NoError(t, json.Unmarshal(listRes.Data, &listPayload))
	require.Len(t, listPayload.OutboundProxies, 1)
	require.Equal(t, "east-proxy-1-v2", listPayload.OutboundProxies[0].Name)
	require.NotContains(t, listPayload.OutboundProxies[0].MaskedURL, "new-secret-pw")
	require.Len(t, listPayload.OutboundProxyPools, 1)
	require.Equal(t, "pool-west", listPayload.OutboundProxyPools[0].Name)
	require.Equal(t, 1, listPayload.OutboundProxyPools[0].ProxyCount)
	require.Equal(t, 1, listPayload.OutboundProxyPools[0].EnabledProxyCount)

	// Delete succeeds when unreferenced.
	delRes := execGQL(t, server, ownerCtx, `
		mutation DeleteProxy($id: ID!) {
			deleteOutboundProxy(id: $id)
		}
	`, map[string]any{"id": created.ID})
	require.Empty(t, delRes.Errors)
	var delPayload struct {
		DeleteOutboundProxy bool `json:"deleteOutboundProxy"`
	}
	require.NoError(t, json.Unmarshal(delRes.Data, &delPayload))
	require.True(t, delPayload.DeleteOutboundProxy)
}

func TestGraphQL_ChannelSettingsNewEnums_UpsertPolicy_NormalizedSnapshot_AndRawKeyProtection(t *testing.T) {
	probeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"remaining":40,"limit":100,"resetAt":"2026-12-01T00:00:00Z"}`))
	}))
	defer probeServer.Close()

	server, client, ownerCtx, readerCtx, _ := setupCredentialProxyGQLTest(t, httpclient.NewHttpClientWithClient(probeServer.Client()))
	defer client.Close()

	// Create a managed proxy first so we can bind a credential to it via upsertChannelCredentialPolicy.
	proxyRes := execGQL(t, server, ownerCtx, `
		mutation CreateProxy($input: CreateOutboundProxyInput!) {
			createOutboundProxy(input: $input) { id name }
		}
	`, map[string]any{
		"input": map[string]any{
			"name":     "gql-bound-proxy",
			"kind":     "static",
			"proxyUrl": "http://proxy.internal:8080",
			"poolName": "pool-gql",
			"status":   "enabled",
		},
	})
	require.Empty(t, proxyRes.Errors)
	var proxyCreated struct {
		CreateOutboundProxy struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"createOutboundProxy"`
	}
	require.NoError(t, json.Unmarshal(proxyRes.Data, &proxyCreated))
	proxyGUID, err := objects.ParseGUID(proxyCreated.CreateOutboundProxy.ID)
	require.NoError(t, err)

	key1 := "sk-gql-secret-key-1"
	key2 := "sk-gql-secret-key-2"
	fp1 := objects.ChannelAPIKeyFingerprint(key1)
	fp2 := objects.ChannelAPIKeyFingerprint(key2)

	// Verify all new ChannelBalanceProbePreset enum values + ChannelKeySelectionStrategy values are accepted in GraphQL mutations.
	createChRes := execGQL(t, server, ownerCtx, `
		mutation CreateChannel($input: CreateChannelInput!) {
			createChannel(input: $input) {
				id
				name
				settings {
					keySelection {
						strategy
					}
					balanceProbe {
						enabled
						preset
						script {
							mode
							expression
						}
					}
				}
			}
		}
	`, map[string]any{
		"input": map[string]any{
			"type":             "openai",
			"name":             "GQL Credential Policy Channel",
			"baseURL":          probeServer.URL,
			"credentials":      map[string]any{"apiKeys": []string{key1, key2}},
			"supportedModels":  []string{"gpt-4o"},
			"defaultTestModel": "gpt-4o",
			"settings": map[string]any{
				"keySelection": map[string]any{
					"strategy": "quota_reset_priority",
				},
				"balanceProbe": map[string]any{
					"enabled": true,
					"preset":  "tavily_usage",
				},
			},
		},
	})
	require.Empty(t, createChRes.Errors)
	var createChPayload struct {
		CreateChannel struct {
			ID       string `json:"id"`
			Settings struct {
				KeySelection struct {
					Strategy string `json:"strategy"`
				} `json:"keySelection"`
				BalanceProbe struct {
					Enabled bool   `json:"enabled"`
					Preset  string `json:"preset"`
				} `json:"balanceProbe"`
			} `json:"settings"`
		} `json:"createChannel"`
	}
	require.NoError(t, json.Unmarshal(createChRes.Data, &createChPayload))
	channelID := createChPayload.CreateChannel.ID
	require.Equal(t, "quota_reset_priority", createChPayload.CreateChannel.Settings.KeySelection.Strategy)
	require.Equal(t, "tavily_usage", createChPayload.CreateChannel.Settings.BalanceProbe.Preset)

	// Exercise remaining new preset enums on updateChannel.
	for _, preset := range []string{"kimi_code_usages", "exa_team_credits", "firecrawl_credits"} {
		upRes := execGQL(t, server, ownerCtx, `
			mutation UpdatePreset($id: ID!, $input: UpdateChannelInput!) {
				updateChannel(id: $id, input: $input) {
					settings {
						balanceProbe { preset }
					}
				}
			}
		`, map[string]any{
			"id": channelID,
			"input": map[string]any{
				"settings": map[string]any{
					"balanceProbe": map[string]any{
						"enabled": true,
						"preset":  preset,
					},
				},
			},
		})
		require.Empty(t, upRes.Errors, preset)
		var upPayload struct {
			UpdateChannel struct {
				Settings struct {
					BalanceProbe struct {
						Preset string `json:"preset"`
					} `json:"balanceProbe"`
				} `json:"settings"`
			} `json:"updateChannel"`
		}
		require.NoError(t, json.Unmarshal(upRes.Data, &upPayload))
		require.Equal(t, preset, upPayload.UpdateChannel.Settings.BalanceProbe.Preset)
	}

	// Update nested settings to smart strategy + expr script balance probe hitting probeServer.
	expr := `{remaining: body.remaining, limit: body.limit, resetAt: body.resetAt, available: body.remaining > 0, currency: "USD"}`
	updateChRes := execGQL(t, server, ownerCtx, `
		mutation UpdateChannelSettings($id: ID!, $input: UpdateChannelInput!) {
			updateChannel(id: $id, input: $input) {
				id
				settings {
					keySelection { strategy }
					balanceProbe {
						enabled
						preset
						script { mode expression }
					}
				}
			}
		}
	`, map[string]any{
		"id": channelID,
		"input": map[string]any{
			"settings": map[string]any{
				"keySelection": map[string]any{
					"strategy": "smart",
				},
				"balanceProbe": map[string]any{
					"enabled": true,
					"preset":  "custom",
					"script": map[string]any{
						"mode":       "expr",
						"expression": expr,
					},
					"http": map[string]any{
						"method":           "GET",
						"urlMode":          "provider_base_url",
						"path":             "/usage",
						"expectedStatuses": []int{200},
					},
				},
			},
		},
	})
	require.Empty(t, updateChRes.Errors)
	var updatedCh struct {
		UpdateChannel struct {
			Settings struct {
				KeySelection struct {
					Strategy string `json:"strategy"`
				} `json:"keySelection"`
				BalanceProbe struct {
					Enabled bool   `json:"enabled"`
					Preset  string `json:"preset"`
					Script  struct {
						Mode       string `json:"mode"`
						Expression string `json:"expression"`
					} `json:"script"`
				} `json:"balanceProbe"`
			} `json:"settings"`
		} `json:"updateChannel"`
	}
	require.NoError(t, json.Unmarshal(updateChRes.Data, &updatedCh))
	require.Equal(t, "smart", updatedCh.UpdateChannel.Settings.KeySelection.Strategy)
	require.Equal(t, "custom", updatedCh.UpdateChannel.Settings.BalanceProbe.Preset)
	require.Equal(t, "expr", updatedCh.UpdateChannel.Settings.BalanceProbe.Script.Mode)
	require.Equal(t, expr, updatedCh.UpdateChannel.Settings.BalanceProbe.Script.Expression)

	// Run balance probe via GraphQL before binding keys to an unreachable mock proxy,
	// so the probe hits probeServer and stores the normalized BalanceSnapshot.
	probeRes := execGQL(t, server, ownerCtx, `
		mutation RunProbe($channelID: ID!) {
			runChannelAPIKeyHealthCheck(channelID: $channelID, mode: balance_probe) {
				id
				quotaRemainingRatio
				quotaResetAt
				balanceSnapshot {
					used
					limit
					remaining
					usageRatio
					resetAt
					success
					available
				}
			}
		}
	`, map[string]any{"channelID": channelID})
	require.Empty(t, probeRes.Errors)
	var probePayload struct {
		RunChannelAPIKeyHealthCheck []struct {
			ID                  string     `json:"id"`
			QuotaRemainingRatio *float64   `json:"quotaRemainingRatio"`
			QuotaResetAt        *time.Time `json:"quotaResetAt"`
			BalanceSnapshot     *struct {
				Used       *float64   `json:"used"`
				Limit      *float64   `json:"limit"`
				Remaining  *float64   `json:"remaining"`
				UsageRatio *float64   `json:"usageRatio"`
				ResetAt    *time.Time `json:"resetAt"`
				Success    bool       `json:"success"`
				Available  *bool      `json:"available"`
			} `json:"balanceSnapshot"`
		} `json:"runChannelAPIKeyHealthCheck"`
	}
	require.NoError(t, json.Unmarshal(probeRes.Data, &probePayload))
	require.Len(t, probePayload.RunChannelAPIKeyHealthCheck, 2)
	for _, item := range probePayload.RunChannelAPIKeyHealthCheck {
		require.NotNil(t, item.BalanceSnapshot)
		require.True(t, item.BalanceSnapshot.Success)
		require.InDelta(t, 60.0, *item.BalanceSnapshot.Used, 1e-9)
		require.InDelta(t, 100.0, *item.BalanceSnapshot.Limit, 1e-9)
		require.InDelta(t, 40.0, *item.BalanceSnapshot.Remaining, 1e-9)
		require.InDelta(t, 0.6, *item.BalanceSnapshot.UsageRatio, 1e-9)
		require.NotNil(t, item.QuotaRemainingRatio)
		require.InDelta(t, 0.4, *item.QuotaRemainingRatio, 1e-9)
		require.NotNil(t, item.QuotaResetAt)
		require.Equal(t, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), item.QuotaResetAt.UTC())
	}

	// Upsert credential policies for both keys via GraphQL while preserving normalized snapshot.
	upsertRes1 := execGQL(t, server, ownerCtx, `
		mutation UpsertPolicy($channelID: ID!, $input: ChannelCredentialPolicyInput!) {
			upsertChannelCredentialPolicy(channelID: $channelID, input: $input) {
				id
				effectiveProxyName
				quotaRemainingRatio
				quotaResetAt
				policy {
					id
					name
					priority
					weight
					egressMode
					outboundProxyId
					egressFailPolicy
				}
			}
		}
	`, map[string]any{
		"channelID": channelID,
		"input": map[string]any{
			"id":               fp1,
			"name":             "key1-pinned",
			"priority":         25,
			"weight":           3,
			"egressMode":       "pinned_proxy",
			"outboundProxyId":  proxyGUID.ID,
			"egressFailPolicy": "backup_proxy",
		},
	})
	require.Empty(t, upsertRes1.Errors)
	var upPolicy1 struct {
		UpsertChannelCredentialPolicy struct {
			ID                  string     `json:"id"`
			EffectiveProxyName  string     `json:"effectiveProxyName"`
			QuotaRemainingRatio *float64   `json:"quotaRemainingRatio"`
			QuotaResetAt        *time.Time `json:"quotaResetAt"`
			Policy              struct {
				ID               string `json:"id"`
				Name             string `json:"name"`
				Priority         int    `json:"priority"`
				Weight           int    `json:"weight"`
				EgressMode       string `json:"egressMode"`
				OutboundProxyID  *int   `json:"outboundProxyId"`
				EgressFailPolicy string `json:"egressFailPolicy"`
			} `json:"policy"`
		} `json:"upsertChannelCredentialPolicy"`
	}
	require.NoError(t, json.Unmarshal(upsertRes1.Data, &upPolicy1))
	require.Equal(t, fp1, upPolicy1.UpsertChannelCredentialPolicy.ID)
	require.Equal(t, "gql-bound-proxy", upPolicy1.UpsertChannelCredentialPolicy.EffectiveProxyName)
	require.NotNil(t, upPolicy1.UpsertChannelCredentialPolicy.QuotaRemainingRatio)
	require.InDelta(t, 0.4, *upPolicy1.UpsertChannelCredentialPolicy.QuotaRemainingRatio, 1e-9)
	require.NotNil(t, upPolicy1.UpsertChannelCredentialPolicy.QuotaResetAt)
	require.Equal(t, "pinned_proxy", upPolicy1.UpsertChannelCredentialPolicy.Policy.EgressMode)
	require.Equal(t, "backup_proxy", upPolicy1.UpsertChannelCredentialPolicy.Policy.EgressFailPolicy)
	require.Equal(t, proxyGUID.ID, *upPolicy1.UpsertChannelCredentialPolicy.Policy.OutboundProxyID)

	upsertRes2 := execGQL(t, server, ownerCtx, `
		mutation UpsertPolicy($channelID: ID!, $input: ChannelCredentialPolicyInput!) {
			upsertChannelCredentialPolicy(channelID: $channelID, input: $input) {
				id
				policy {
					egressMode
					proxyPoolName
					egressFailPolicy
				}
			}
		}
	`, map[string]any{
		"channelID": channelID,
		"input": map[string]any{
			"id":               fp2,
			"name":             "key2-pool",
			"priority":         10,
			"weight":           1,
			"egressMode":       "proxy_pool",
			"proxyPoolName":    "pool-gql",
			"egressFailPolicy": "pool_fallback",
		},
	})
	require.Empty(t, upsertRes2.Errors)

	// Deleting a referenced managed proxy must fail while bound to fp1.
	blockedDelete := execGQL(t, server, ownerCtx, `
		mutation DeleteProxy($id: ID!) {
			deleteOutboundProxy(id: $id)
		}
	`, map[string]any{"id": proxyCreated.CreateOutboundProxy.ID})
	require.NotEmpty(t, blockedDelete.Errors)

	// Verify channelAPIKeyInventory protects rawKey for read_channels-only user, and reveals it for write_channels user.
	inventoryQuery := `
		query Inventory($channelID: ID!) {
			channelAPIKeyInventory(channelID: $channelID) {
				id
				rawKey
				maskedKey
				quotaRemainingRatio
				quotaResetAt
				policy {
					id
					egressMode
					egressFailPolicy
				}
				balanceSnapshot {
					used
					limit
					remaining
					usageRatio
				}
			}
		}
	`
	readerInvRes := execGQL(t, server, readerCtx, inventoryQuery, map[string]any{"channelID": channelID})
	require.Empty(t, readerInvRes.Errors)
	var readerInv struct {
		ChannelAPIKeyInventory []struct {
			ID                  string   `json:"id"`
			RawKey              *string  `json:"rawKey"`
			MaskedKey           string   `json:"maskedKey"`
			QuotaRemainingRatio *float64 `json:"quotaRemainingRatio"`
		} `json:"channelAPIKeyInventory"`
	}
	require.NoError(t, json.Unmarshal(readerInvRes.Data, &readerInv))
	require.Len(t, readerInv.ChannelAPIKeyInventory, 2)
	for _, item := range readerInv.ChannelAPIKeyInventory {
		require.Nil(t, item.RawKey)
		require.NotEmpty(t, item.MaskedKey)
		require.NotNil(t, item.QuotaRemainingRatio)
		require.InDelta(t, 0.4, *item.QuotaRemainingRatio, 1e-9)
	}

	ownerInvRes := execGQL(t, server, ownerCtx, inventoryQuery, map[string]any{"channelID": channelID})
	require.Empty(t, ownerInvRes.Errors)
	var ownerInv struct {
		ChannelAPIKeyInventory []struct {
			ID     string  `json:"id"`
			RawKey *string `json:"rawKey"`
		} `json:"channelAPIKeyInventory"`
	}
	require.NoError(t, json.Unmarshal(ownerInvRes.Data, &ownerInv))
	require.Len(t, ownerInv.ChannelAPIKeyInventory, 2)
	require.Equal(t, key1, *ownerInv.ChannelAPIKeyInventory[0].RawKey)
	require.Equal(t, key2, *ownerInv.ChannelAPIKeyInventory[1].RawKey)

	// Verify queryChannels echoes nested settings + credentialPolicies + proxy reference counts.
	channelsRes := execGQL(t, server, readerCtx, `
		query Channels {
			queryChannels(input: { first: 10 }) {
				edges {
					node {
						id
						settings {
							keySelection { strategy }
							balanceProbe {
								preset
								script { mode expression }
							}
							credentialPolicies {
								id
								egressMode
								egressFailPolicy
								outboundProxyId
								proxyPoolName
							}
						}
					}
				}
			}
			outboundProxies {
				id
				credentialReferenceCount
				primaryCredentialCount
				channelReferenceCount
			}
			outboundProxyPools {
				name
				credentialReferenceCount
			}
		}
	`, nil)
	require.Empty(t, channelsRes.Errors)
	var channelsPayload struct {
		QueryChannels struct {
			Edges []struct {
				Node struct {
					Settings struct {
						KeySelection struct {
							Strategy string `json:"strategy"`
						} `json:"keySelection"`
						BalanceProbe struct {
							Preset string `json:"preset"`
							Script struct {
								Mode       string `json:"mode"`
								Expression string `json:"expression"`
							} `json:"script"`
						} `json:"balanceProbe"`
						CredentialPolicies []struct {
							ID               string `json:"id"`
							EgressMode       string `json:"egressMode"`
							EgressFailPolicy string `json:"egressFailPolicy"`
						} `json:"credentialPolicies"`
					} `json:"settings"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"queryChannels"`
		OutboundProxies []struct {
			CredentialReferenceCount int `json:"credentialReferenceCount"`
			PrimaryCredentialCount   int `json:"primaryCredentialCount"`
			ChannelReferenceCount    int `json:"channelReferenceCount"`
		} `json:"outboundProxies"`
		OutboundProxyPools []struct {
			Name                     string `json:"name"`
			CredentialReferenceCount int    `json:"credentialReferenceCount"`
		} `json:"outboundProxyPools"`
	}
	require.NoError(t, json.Unmarshal(channelsRes.Data, &channelsPayload))
	require.Len(t, channelsPayload.QueryChannels.Edges, 1)
	nodeSettings := channelsPayload.QueryChannels.Edges[0].Node.Settings
	require.Equal(t, "smart", nodeSettings.KeySelection.Strategy)
	require.Equal(t, "custom", nodeSettings.BalanceProbe.Preset)
	require.Equal(t, "expr", nodeSettings.BalanceProbe.Script.Mode)
	require.Len(t, nodeSettings.CredentialPolicies, 2)
	require.Len(t, channelsPayload.OutboundProxies, 1)
	require.Equal(t, 1, channelsPayload.OutboundProxies[0].CredentialReferenceCount)
	require.Equal(t, 1, channelsPayload.OutboundProxies[0].PrimaryCredentialCount)
	require.Equal(t, 1, channelsPayload.OutboundProxies[0].ChannelReferenceCount)
	require.Len(t, channelsPayload.OutboundProxyPools, 1)
	require.Equal(t, "pool-gql", channelsPayload.OutboundProxyPools[0].Name)
	require.Equal(t, 2, channelsPayload.OutboundProxyPools[0].CredentialReferenceCount)
}

func TestGraphQL_APIKey_ExposeUpstreamKeyQuota_ToggleEcho(t *testing.T) {
	server, client, ownerCtx, _, testProject := setupCredentialProxyGQLTest(t, nil)
	defer client.Close()

	projectGUID := fmt.Sprintf("gid://axonhub/Project/%d", testProject.ID)

	// Create omitting exposeUpstreamKeyQuota -> defaults to true.
	createDefaultRes := execGQL(t, server, ownerCtx, `
		mutation CreateAPIKey($input: CreateAPIKeyInput!) {
			createAPIKey(input: $input) {
				id
				name
				exposeUpstreamKeyQuota
			}
		}
	`, map[string]any{
		"input": map[string]any{
			"name":      "gql-key-default",
			"projectID": projectGUID,
		},
	})
	require.Empty(t, createDefaultRes.Errors)
	var defaultPayload struct {
		CreateAPIKey struct {
			ID                     string `json:"id"`
			Name                   string `json:"name"`
			ExposeUpstreamKeyQuota bool   `json:"exposeUpstreamKeyQuota"`
		} `json:"createAPIKey"`
	}
	require.NoError(t, json.Unmarshal(createDefaultRes.Data, &defaultPayload))
	require.True(t, defaultPayload.CreateAPIKey.ExposeUpstreamKeyQuota)

	// Create with exposeUpstreamKeyQuota: false -> echoes false.
	createFalseRes := execGQL(t, server, ownerCtx, `
		mutation CreateAPIKey($input: CreateAPIKeyInput!) {
			createAPIKey(input: $input) {
				id
				name
				exposeUpstreamKeyQuota
			}
		}
	`, map[string]any{
		"input": map[string]any{
			"name":                   "gql-key-false",
			"projectID":              projectGUID,
			"exposeUpstreamKeyQuota": false,
		},
	})
	require.Empty(t, createFalseRes.Errors)
	var falsePayload struct {
		CreateAPIKey struct {
			ID                     string `json:"id"`
			Name                   string `json:"name"`
			ExposeUpstreamKeyQuota bool   `json:"exposeUpstreamKeyQuota"`
		} `json:"createAPIKey"`
	}
	require.NoError(t, json.Unmarshal(createFalseRes.Data, &falsePayload))
	require.False(t, falsePayload.CreateAPIKey.ExposeUpstreamKeyQuota)

	// Update false -> true, then default true -> false, verifying echo on both mutations and list query.
	updateTrueRes := execGQL(t, server, ownerCtx, `
		mutation UpdateAPIKey($id: ID!, $input: UpdateAPIKeyInput!) {
			updateAPIKey(id: $id, input: $input) {
				id
				exposeUpstreamKeyQuota
			}
		}
	`, map[string]any{
		"id": falsePayload.CreateAPIKey.ID,
		"input": map[string]any{
			"exposeUpstreamKeyQuota": true,
		},
	})
	require.Empty(t, updateTrueRes.Errors)
	var updatedTrue struct {
		UpdateAPIKey struct {
			ExposeUpstreamKeyQuota bool `json:"exposeUpstreamKeyQuota"`
		} `json:"updateAPIKey"`
	}
	require.NoError(t, json.Unmarshal(updateTrueRes.Data, &updatedTrue))
	require.True(t, updatedTrue.UpdateAPIKey.ExposeUpstreamKeyQuota)

	updateFalseRes := execGQL(t, server, ownerCtx, `
		mutation UpdateAPIKey($id: ID!, $input: UpdateAPIKeyInput!) {
			updateAPIKey(id: $id, input: $input) {
				id
				exposeUpstreamKeyQuota
			}
		}
	`, map[string]any{
		"id": defaultPayload.CreateAPIKey.ID,
		"input": map[string]any{
			"exposeUpstreamKeyQuota": false,
		},
	})
	require.Empty(t, updateFalseRes.Errors)
	var updatedFalse struct {
		UpdateAPIKey struct {
			ExposeUpstreamKeyQuota bool `json:"exposeUpstreamKeyQuota"`
		} `json:"updateAPIKey"`
	}
	require.NoError(t, json.Unmarshal(updateFalseRes.Data, &updatedFalse))
	require.False(t, updatedFalse.UpdateAPIKey.ExposeUpstreamKeyQuota)

	listRes := execGQL(t, server, ownerCtx, `
		query ListAPIKeys {
			apiKeys(first: 10) {
				edges {
					node {
						name
						exposeUpstreamKeyQuota
					}
				}
			}
		}
	`, nil)
	require.Empty(t, listRes.Errors)
	var listPayload struct {
		APIKeys struct {
			Edges []struct {
				Node struct {
					Name                   string `json:"name"`
					ExposeUpstreamKeyQuota bool   `json:"exposeUpstreamKeyQuota"`
				} `json:"node"`
			} `json:"edges"`
		} `json:"apiKeys"`
	}
	require.NoError(t, json.Unmarshal(listRes.Data, &listPayload))
	got := map[string]bool{}
	for _, edge := range listPayload.APIKeys.Edges {
		got[edge.Node.Name] = edge.Node.ExposeUpstreamKeyQuota
	}
	require.Equal(t, map[string]bool{
		"gql-key-default": false,
		"gql-key-false":   true,
	}, got)
}
