package diagnostics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestPullFidelityOrderingPaginationAndCredentialExclusion(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:diagnostics-service?mode=memory&_fk=1")
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	systems := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	channels := biz.NewChannelServiceForTest(client)
	usage := biz.NewUsageLogService(client, systems, channels)
	storage := biz.NewDataStorageService(biz.DataStorageServiceParams{SystemService: systems, CacheConfig: xcache.Config{}, Client: client})
	requests := biz.NewRequestService(client, systems, usage, storage, biz.NewLiveStreamRegistry())
	service := NewService(Params{Ent: client, Requests: requests, Systems: systems, Storage: storage})
	client.System.Create().SetKey(biz.SystemKeySecretKey).SetValue("cursor-secret").SaveX(setupCtx)
	client.DataStorage.Create().SetName("Primary").SetDescription("Diagnostics test storage").SetPrimary(true).SetType(datastorage.TypeDatabase).SetStatus(datastorage.StatusActive).SetSettings(&objects.DataStorageSettings{}).SaveX(setupCtx)
	p := client.Project.Create().SetName("diagnostics-project").SetStatus(project.StatusActive).SaveX(setupCtx)
	u := client.User.Create().SetEmail("owner@example.com").SetPassword("x").SetStatus(user.StatusActivated).SetIsOwner(true).SaveX(setupCtx)
	key := client.APIKey.Create().SetProject(p).SetUser(u).SetName("admin-key").SetKey("reusable-secret").SetType(apikey.TypeServiceAccount).SetProvisioningSource(apikey.ProvisioningSourceAdmin).SetScopes([]string{string(scopes.ScopeReadDiagnostics), string(scopes.ScopeReadAPIKeys)}).SaveX(setupCtx)
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		client.Request.Create().SetProject(p).SetAPIKeyID(key.ID).SetModelID("model").SetRequestHeaders([]byte(`{"X-Test":["keep"]}`)).SetRequestBody([]byte(fmt.Sprintf(`{"index":%d,"large":9007199254740991,"nested":{"token":"ordinary-evidence"}}`, i))).SetResponseBody([]byte(`{"ok":true}`)).SetStatus(request.StatusCompleted).SetCreatedAt(base.Add(time.Duration(i) * time.Second)).SetUpdatedAt(base.Add(time.Duration(i) * time.Second)).SaveX(setupCtx)
	}
	ctx := authz.NewUserContext(context.Background(), u.ID)
	ctx = contexts.WithUser(ctx, u)
	ctx = contexts.WithProjectID(ctx, p.ID)
	req := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: p.ID}, Selector: Selector{Kind: "timeRange", From: base.Add(-time.Second).Format(time.RFC3339Nano), To: base.Add(time.Minute).Format(time.RFC3339Nano)}, Include: Include{Sections: []string{"requests"}}, Limits: Limits{MaxRequests: 2}}
	page1, err := service.Pull(ctx, req)
	require.NoError(t, err)
	page1JSON, err := json.Marshal(page1)
	require.NoError(t, err)
	require.NoError(t, ValidatePullResponseJSON(page1JSON))
	require.True(t, page1.Selection.HasMore)
	require.NotNil(t, page1.Selection.NextCursor)
	require.Equal(t, 2, page1.Selection.Counts.Requests)
	rows := page1.Sections.Requests.Data.([]any)
	requireContractDecode[[]RequestRecordContract](t, page1.Sections.Requests.Data)
	first := rows[0].(map[string]any)
	evidence := first["requestBody"].(Evidence)
	decoder := json.NewDecoder(bytes.NewReader(evidence.Value.(json.RawMessage)))
	decoder.UseNumber()
	var value map[string]any
	require.NoError(t, decoder.Decode(&value))
	require.Equal(t, "ordinary-evidence", value["nested"].(map[string]any)["token"])
	require.Equal(t, "9007199254740991", value["large"].(json.Number).String())
	require.Equal(t, "rfc8785", evidence.Canonicalization)
	encodedEvidence, err := json.Marshal(evidence)
	require.NoError(t, err)
	require.Contains(t, string(encodedEvidence), `"large":9007199254740991`)
	require.Equal(t, "available", evidence.State)
	req.Page = &Page{Cursor: page1.Selection.NextCursor}
	page2, err := service.Pull(ctx, req)
	require.NoError(t, err)
	require.Equal(t, page1.Bundle.ID, page2.Bundle.ID)
	require.Equal(t, 2, page2.Bundle.PageIndex)
	require.False(t, page2.Selection.HasMore)
	require.Equal(t, 1, page2.Selection.Counts.Requests)
	req.Selector.ModelIDs = []string{"changed"}
	_, err = service.Pull(ctx, req)
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	require.Equal(t, "CURSOR_MISMATCH", serviceErr.Code)

	largeRaw := []byte(`{"large":9007199254740993,"lexical":1.0}`)
	largeRequest := client.Request.Create().SetProject(p).SetAPIKeyID(key.ID).SetModelID("model").SetRequestBody(largeRaw).SetStatus(request.StatusCompleted).SaveX(setupCtx)
	largeBundle, err := service.Pull(ctx, PullRequest{
		Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0},
		Scope:    Scope{ProjectID: p.ID},
		Selector: Selector{Kind: "requestIds", IDs: json.RawMessage(fmt.Sprintf("[%d]", largeRequest.ID))},
		Include:  Include{Sections: []string{"requests"}},
	})
	require.NoError(t, err)
	require.Equal(t, "partial", largeBundle.Bundle.Status)
	require.Equal(t, "partial", largeBundle.Sections.Requests.Status)
	require.NotEmpty(t, largeBundle.Issues)
	require.Equal(t, "EVIDENCE_CANONICALIZATION_UNAVAILABLE", largeBundle.Issues[0].Code)
	largeRecord := largeBundle.Sections.Requests.Data.([]any)[0].(map[string]any)
	largeEvidence := largeRecord["requestBody"].(Evidence)
	require.Equal(t, "available", largeEvidence.State)
	require.Equal(t, "unavailable", largeEvidence.CanonicalizationStatus)
	require.Empty(t, largeEvidence.CanonicalSHA256)
	require.Equal(t, string(largeRaw), string(largeEvidence.Value.(json.RawMessage)))
	rawHash := sha256.Sum256(largeRaw)
	require.Equal(t, hex.EncodeToString(rawHash[:]), largeEvidence.RawSHA256)
	largeJSON, err := json.Marshal(largeBundle)
	require.NoError(t, err)
	require.NoError(t, ValidatePullResponseJSON(largeJSON))
	require.Contains(t, string(largeJSON), `"large":9007199254740993`)
	contractBundle, err := PullResponseToContract(largeBundle)
	require.NoError(t, err)
	contractJSON, err := json.Marshal(contractBundle)
	require.NoError(t, err)
	require.Contains(t, string(contractJSON), string(largeRaw))
	require.NoError(t, ValidatePullResponseJSON(contractJSON))

	selectedID := client.Request.Query().Order(ent.Asc(request.FieldID)).FirstIDX(setupCtx)
	metadataOnly := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: p.ID}, Selector: Selector{Kind: "requestIds", IDs: json.RawMessage(fmt.Sprintf("[%d]", selectedID))}, Include: Include{Sections: []string{"apiKeys"}}}
	metadataBundle, err := service.Pull(ctx, metadataOnly)
	require.NoError(t, err)
	require.Equal(t, 1, metadataBundle.Selection.Counts.APIKeys)
	require.Len(t, metadataBundle.Sections.APIKeys.Data.([]any), 1)
	healthOnly := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: p.ID}, Selector: Selector{Kind: "snapshot"}, Include: Include{Sections: []string{"health"}}}
	healthBundle, err := service.Pull(ctx, healthOnly)
	require.NoError(t, err)
	require.Equal(t, "available", healthBundle.Sections.Health.Status)
	require.Equal(t, "available", healthBundle.Sections.Health.Data.(map[string]any)["databaseStatus"])
	require.IsType(t, int64(0), healthBundle.Sections.Health.Data.(map[string]any)["databaseLatencyMs"])

	snapshot := PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: p.ID}, Selector: Selector{Kind: "snapshot"}, Include: Include{Sections: []string{"apiKeys"}}}
	bundle, err := service.Pull(ctx, snapshot)
	require.NoError(t, err)
	keys := bundle.Sections.APIKeys.Data.([]any)
	record := keys[0].(map[string]any)
	requireContractDecode[[]APIKeyRecordContract](t, bundle.Sections.APIKeys.Data)
	credential := record["key"].(Credential)
	require.Equal(t, "excluded", credential.Status)
	require.Empty(t, credential.Value)
	excludedCredentialJSON, err := json.Marshal(credential)
	require.NoError(t, err)
	require.NotContains(t, string(excludedCredentialJSON), `"value"`)
	defaultBundleJSON, err := json.Marshal(bundle)
	require.NoError(t, err)
	require.NotContains(t, string(defaultBundleJSON), "reusable-secret")
	require.NoError(t, ValidatePullResponseJSON(defaultBundleJSON))

	var invalidCredentialResponse map[string]any
	require.NoError(t, json.Unmarshal(defaultBundleJSON, &invalidCredentialResponse))
	apiKeysSection := invalidCredentialResponse["sections"].(map[string]any)["apiKeys"].(map[string]any)
	keyCredential := apiKeysSection["data"].([]any)[0].(map[string]any)["key"].(map[string]any)
	keyCredential["value"] = "must-not-be-allowed"
	invalidExcludedJSON, err := json.Marshal(invalidCredentialResponse)
	require.NoError(t, err)
	require.Error(t, ValidatePullResponseJSON(invalidExcludedJSON))
	keyCredential["status"] = "included"
	delete(keyCredential, "value")
	invalidIncludedJSON, err := json.Marshal(invalidCredentialResponse)
	require.NoError(t, err)
	require.Error(t, ValidatePullResponseJSON(invalidIncludedJSON))

	snapshot.Include.Credentials = true
	includedBundle, err := service.Pull(ctx, snapshot)
	require.NoError(t, err)
	require.True(t, includedBundle.Authorization.CredentialsIncluded)
	includedRecord := includedBundle.Sections.APIKeys.Data.([]any)[0].(map[string]any)
	includedCredential := includedRecord["key"].(Credential)
	require.Equal(t, "included", includedCredential.Status)
	require.Equal(t, "reusable-secret", includedCredential.Value)
	includedBundleJSON, err := json.Marshal(includedBundle)
	require.NoError(t, err)
	require.NoError(t, ValidatePullResponseJSON(includedBundleJSON))

	legacy := client.APIKey.Create().SetProject(p).SetUser(u).SetName("legacy-user-key").SetKey("legacy-secret").SetType(apikey.TypeUser).SaveX(setupCtx)
	serviceCtx := authz.NewAPIKeyContext(context.Background(), key.ID, p.ID)
	serviceCtx = contexts.WithAPIKey(serviceCtx, key)
	serviceCtx = contexts.WithProjectID(serviceCtx, p.ID)
	_, err = service.Pull(serviceCtx, snapshot)
	var credentialErr *ServiceError
	require.ErrorAs(t, err, &credentialErr)
	require.Equal(t, "CREDENTIAL_EXPORT_FORBIDDEN", credentialErr.Code)
	snapshot.Include.Credentials = false
	serviceBundle, err := service.Pull(serviceCtx, snapshot)
	require.NoError(t, err)
	for _, item := range serviceBundle.Sections.APIKeys.Data.([]any) {
		require.NotEqual(t, legacy.ID, item.(map[string]any)["id"])
	}

	captured := client.Request.Create().SetProject(p).SetAPIKeyID(key.ID).SetModelID("model").SetRequestBody([]byte(`{"request":true}`)).SetStatus(request.StatusProcessing).SaveX(setupCtx)
	require.NoError(t, requests.UpdateRequestCompleted(setupCtx, captured.ID, "external", map[string]any{"response": true}, nil))
	captured = client.Request.GetX(setupCtx, captured.ID)
	require.NotNil(t, captured.EvidenceDisposition)
	require.Equal(t, "persist", captured.EvidenceDisposition.ResponseBody.Intent)
	require.Equal(t, "database", captured.EvidenceDisposition.ResponseBody.Location)
	require.Equal(t, "stored", captured.EvidenceDisposition.ResponseBody.Outcome)
}

func TestPullChannelSnapshotIsProjectDerivedAndCredentialSafe(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:diagnostics-channel-snapshot?mode=memory&_fk=1")
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	systems := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	service := NewService(Params{Ent: client, Systems: systems})
	client.System.Create().SetKey(biz.SystemKeySecretKey).SetValue("cursor-secret").SaveX(setupCtx)
	p := client.Project.Create().SetName("diagnostics-project").SetStatus(project.StatusActive).SaveX(setupCtx)
	u := client.User.Create().SetEmail("owner@example.com").SetPassword("x").SetStatus(user.StatusActivated).SetIsOwner(true).SaveX(setupCtx)
	relevant := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("project-channel").
		SetBaseURL("https://url-user:url-password@example.test/v1?token=url-query-secret").
		SetCredentials(objects.ChannelCredentials{APIKey: "channel-api-secret"}).
		SetSupportedModels([]string{"model"}).
		SetDefaultTestModel("model").
		SetSettings(&objects.ChannelSettings{
			BodyOverrideOperations: []objects.OverrideOperation{{Op: objects.OverrideOpSet, Path: "secret.path", Value: "override-secret"}, {Op: objects.OverrideOpArrayRemove, Path: "items", Match: &objects.OverrideMatch{Path: "token", Eq: "match-secret"}}},
			OverrideHeaders:        []objects.HeaderEntry{{Key: "Authorization", Value: "header-secret"}},
			Proxy:                  &objects.ProxyConfig{Type: objects.ProxyType("url"), URL: "http://proxy-user:proxy-pass@example.test?key=proxy-query", Username: "proxy-user-secret", Password: "proxy-password-secret"},
			ProviderQuota:          &objects.ChannelProviderQuotaSettings{OpencodeGo: &objects.OpenCodeGoQuotaSettings{WorkspaceID: "workspace", AuthCookie: "cookie-secret"}},
		}).
		SetEndpoints([]objects.ChannelEndpoint{{APIFormat: "openai", BaseURL: "https://endpoint-user:endpoint-pass@example.test/v1?key=endpoint-query"}}).
		SaveX(setupCtx)
	client.Channel.Create().SetType(channel.TypeOpenai).SetName("unrelated-channel").SetCredentials(objects.ChannelCredentials{APIKey: "unrelated-secret"}).SetSupportedModels([]string{"other"}).SetDefaultTestModel("other").SaveX(setupCtx)
	client.APIKey.Create().SetProject(p).SetUser(u).SetName("admin-key").SetKey("api-key-secret").SetType(apikey.TypeServiceAccount).SetProvisioningSource(apikey.ProvisioningSourceAdmin).SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "limited", Profiles: []objects.APIKeyProfile{{Name: "limited", ChannelIDs: []int{relevant.ID}}}}).SaveX(setupCtx)

	ctx := authz.NewUserContext(context.Background(), u.ID)
	ctx = contexts.WithUser(ctx, u)
	ctx = contexts.WithProjectID(ctx, p.ID)
	bundle, err := service.Pull(ctx, PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: p.ID}, Selector: Selector{Kind: "snapshot"}, Include: Include{Sections: []string{"channels"}}})
	require.NoError(t, err)
	require.Equal(t, 1, bundle.Selection.Counts.Channels)
	requireContractDecode[[]ChannelRecordContract](t, bundle.Sections.Channels.Data)
	encoded, err := json.Marshal(bundle)
	require.NoError(t, err)
	payload := string(encoded)
	for _, secret := range []string{"channel-api-secret", "unrelated-secret", "url-user", "url-password", "url-query-secret", "override-secret", "match-secret", "header-secret", "proxy-user-secret", "proxy-password-secret", "proxy-user", "proxy-pass", "proxy-query", "cookie-secret", "endpoint-user", "endpoint-pass", "endpoint-query"} {
		require.Falsef(t, strings.Contains(payload, secret), "credential %q leaked in diagnostics payload", secret)
	}
	require.Contains(t, payload, `"path":"secret.path"`)
	require.Contains(t, payload, `"status":"excluded"`)

	configurationBundle, err := service.Pull(ctx, PullRequest{Contract: ContractRequest{Name: ContractName, Major: 1, MinMinor: 0, MaxMinor: 0}, Scope: Scope{ProjectID: p.ID}, Selector: Selector{Kind: "snapshot"}, Include: Include{Sections: []string{"configuration"}}})
	require.NoError(t, err)
	requireContractDecode[ConfigurationDataContract](t, configurationBundle.Sections.Configuration.Data)
}

func requireContractDecode[T any](t *testing.T, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decoded T
	require.NoError(t, decoder.Decode(&decoded), string(raw))
	require.ErrorIs(t, decoder.Decode(&struct{}{}), io.EOF)
}
