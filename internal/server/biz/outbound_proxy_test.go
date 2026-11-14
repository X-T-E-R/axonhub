package biz

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/migrate/schemahook"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

type outboundProxyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f outboundProxyRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func setupOutboundProxyService(t *testing.T) (*OutboundProxyService, *ent.Client, context.Context) {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:outbound-proxy?mode=memory&_fk=1")
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	return NewOutboundProxyService(OutboundProxyServiceParams{Ent: client}), client, ctx
}

func setupOutboundProxyForeignKeyClient(t *testing.T) (*ent.Client, context.Context) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", "file:outbound-proxy-fk?mode=memory&_fk=1")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	client := enttest.NewClient(t,
		enttest.WithOptions(ent.Driver(entsql.OpenDB("sqlite3", sqlDB))),
		enttest.WithMigrateOptions(
			migrate.WithGlobalUniqueID(false),
			migrate.WithForeignKeys(true),
			migrate.WithDropIndex(true),
			migrate.WithDropColumn(true),
			schema.WithHooks(schemahook.V0_3_0),
		),
	)
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	return client, ctx
}

func TestOutboundProxyDatabaseForeignKeyRestrictsBackupDelete(t *testing.T) {
	client, ctx := setupOutboundProxyForeignKeyClient(t)
	defer client.Close()

	backup := client.OutboundProxy.Create().
		SetName("database-backup").
		SetProxyURL("http://backup.example:8080").
		SaveX(ctx)
	primary := client.OutboundProxy.Create().
		SetName("database-primary").
		SetProxyURL("http://primary.example:8080").
		SetBackupProxyID(backup.ID).
		SaveX(ctx)

	require.Error(t, client.OutboundProxy.DeleteOneID(backup.ID).Exec(ctx))
	require.NoError(t, client.OutboundProxy.UpdateOneID(primary.ID).ClearBackupProxyID().Exec(ctx))
	require.NoError(t, client.OutboundProxy.DeleteOneID(backup.ID).Exec(ctx))
}

func TestOutboundProxyWriteContextRequiresWriteScope(t *testing.T) {
	_, err := outboundProxyWriteContext(context.Background())
	require.Error(t, err)
}

func TestValidateOutboundProxyURLAllowsPrivateResinEndpoint(t *testing.T) {
	require.NoError(t, ValidateOutboundProxyURL(OutboundProxyKindStatic, "http://Platform.key:token@127.0.0.1:2260"))
	require.NoError(t, ValidateOutboundProxyURL(OutboundProxyKindDynamicTemplate, "http://Platform.{credential_id}:token@127.0.0.1:2260"))
	require.Error(t, ValidateOutboundProxyURL(OutboundProxyKindStatic, "http://Platform.{credential_id}:token@127.0.0.1:2260"))
	require.Error(t, ValidateOutboundProxyURL(OutboundProxyKindStatic, "file:///tmp/proxy"))
	require.Error(t, ValidateOutboundProxyURL(OutboundProxyKindDynamicTemplate, "http://user:pass@{credential_id}:2260"))
	require.Error(t, ValidateOutboundProxyURL(OutboundProxyKindDynamicTemplate, "http://user:pass@127.0.0.1:2260/{unknown}"))
}

func TestRenderOutboundProxyURLEscapesTemplateValuesAndKeepsHost(t *testing.T) {
	rendered, err := RenderOutboundProxyURL(
		"http://user-{credential_id}:pass@127.0.0.1:2260/{session}?channel={channel_id}",
		"key/a", "session value", "channel?1",
	)
	require.NoError(t, err)
	require.Contains(t, rendered, "user-key%2Fa")
	require.Contains(t, rendered, "session%20value")
	require.Contains(t, rendered, "channel%3F1")
	require.Contains(t, rendered, "127.0.0.1:2260")
}

func TestMaskOutboundProxyURLNeverReturnsProxyCredentials(t *testing.T) {
	masked := maskOutboundProxyURL("http://user:password@resin.example:2260/private/path?token=query-secret")
	require.NotContains(t, masked, "user")
	require.NotContains(t, masked, "password")
	require.NotContains(t, masked, "query-secret")
	require.NotContains(t, masked, "private")
	require.Equal(t, "http://redacted@resin.example:2260/redacted", masked)
}

func TestParseOutboundProxyProbeResponsesRequiresPublicIP(t *testing.T) {
	exitIP, country, err := parseCloudflareTraceProbeResponse([]byte("ip=203.0.113.10\nloc=US\n"))
	require.NoError(t, err)
	require.Equal(t, "203.0.113.10", exitIP)
	require.Equal(t, "US", country)

	_, _, err = parseCloudflareTraceProbeResponse([]byte("ip=127.0.0.1\nloc=US\n"))
	require.Error(t, err)

	exitIP, _, err = parseIPifyProbeResponse([]byte(`{"ip":"198.51.100.20"}`))
	require.NoError(t, err)
	require.Equal(t, "198.51.100.20", exitIP)
}

func TestOutboundProxyProbeDisablesRedirectsAndRedactsErrors(t *testing.T) {
	var calls atomic.Int32
	svc := &OutboundProxyService{
		probeTransport: outboundProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://secret.invalid/?password=secret"}},
				Body:       io.NopCloser(strings.NewReader("redirected")),
				Request:    req,
			}, nil
		}),
	}

	result, err := svc.probeThroughProxy(t.Context(), "http://user:password@127.0.0.1:2260")
	require.NoError(t, err)
	require.False(t, result.success)
	require.NotContains(t, result.errText, "password")
	require.NotContains(t, result.errText, "secret.invalid")
	require.LessOrEqual(t, calls.Load(), int32(2))
}

func TestOutboundProxyServiceCRUDRedactsAndResetsProbeState(t *testing.T) {
	svc, client, ctx := setupOutboundProxyService(t)
	defer client.Close()

	created, err := svc.Create(ctx, OutboundProxyCreateInput{
		Name:     "resin-sidecar",
		ProxyURL: "http://proxy-user:super-secret@127.0.0.1:2260/private/path?token=query-secret",
		PoolName: "local-residential",
	})
	require.NoError(t, err)
	require.Equal(t, "local-residential", created.PoolName)
	require.NotContains(t, created.MaskedURL, "super-secret")
	require.NotContains(t, created.MaskedURL, "query-secret")
	encoded, err := json.Marshal(created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "super-secret")
	require.NotContains(t, string(encoded), "query-secret")

	stored := client.OutboundProxy.GetX(ctx, created.ID)
	require.Contains(t, stored.ProxyURL, "super-secret")
	_, err = client.OutboundProxy.UpdateOneID(created.ID).
		SetLastTestedAt(time.Now()).
		SetLastTestSuccess(true).
		SetLastExitIP("203.0.113.1").
		SetLastCountry("US").
		SetLastLatencyMs(12).
		SetLastTestError("").
		Save(ctx)
	require.NoError(t, err)

	remark := "edited without replacing secret"
	updated, err := svc.Update(ctx, created.ID, OutboundProxyUpdateInput{Remark: &remark})
	require.NoError(t, err)
	require.Equal(t, "local-residential", updated.PoolName)
	require.Equal(t, remark, updated.Remark)
	stored = client.OutboundProxy.GetX(ctx, created.ID)
	require.Contains(t, stored.ProxyURL, "super-secret")
	require.NotNil(t, updated.LastTestSuccess)
	require.True(t, *updated.LastTestSuccess)

	newURL := "https://next-proxy.example:443"
	updated, err = svc.Update(ctx, created.ID, OutboundProxyUpdateInput{ProxyURL: &newURL})
	require.NoError(t, err)
	require.Nil(t, updated.LastTestedAt)
	require.Nil(t, updated.LastTestSuccess)
	require.Empty(t, updated.LastExitIP)
	require.Empty(t, updated.LastCountry)
	require.Nil(t, updated.LastLatencyMs)
	require.Empty(t, updated.LastTestError)

	require.NoError(t, svc.Delete(ctx, created.ID))
	require.Equal(t, 0, client.OutboundProxy.Query().CountX(ctx))
}

func TestOutboundProxyServiceCountsReferencesEnforcesRestrictionsAndDetectsCycles(t *testing.T) {
	svc, client, ctx := setupOutboundProxyService(t)
	defer client.Close()

	credentialLimit := 2
	backup, err := svc.Create(ctx, OutboundProxyCreateInput{
		Name:           "backup",
		ProxyURL:       "http://backup.example:8080",
		PoolName:       "us-residential",
		MaxCredentials: &credentialLimit,
	})
	require.NoError(t, err)
	primary, err := svc.Create(ctx, OutboundProxyCreateInput{
		Name:          "primary",
		ProxyURL:      "http://primary.example:8080",
		PoolName:      "us-residential",
		BackupProxyID: &backup.ID,
	})
	require.NoError(t, err)

	primaryID := primary.ID
	settings := &objects.ChannelSettings{CredentialPolicies: []objects.ChannelCredentialPolicy{
		{ID: "key_one", OutboundProxyID: &primaryID, EgressFailPolicy: objects.CredentialEgressFailPolicyBackupProxy},
		{ID: "key_two", OutboundProxyID: &primaryID, EgressFailPolicy: objects.CredentialEgressFailPolicyBackupProxy},
	}}
	client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("proxy-reference-channel").
		SetBaseURL("https://unused.example.test").
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-key"}).
		SetSupportedModels([]string{"m"}).
		SetDefaultTestModel("m").
		SetSettings(settings).
		SaveX(ctx)

	listed, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	byID := make(map[int]*OutboundProxyDTO, len(listed))
	for _, item := range listed {
		byID[item.ID] = item
	}
	require.Equal(t, 2, byID[primary.ID].PrimaryCredentialCount)
	require.Equal(t, 2, byID[primary.ID].CredentialReferenceCount)
	require.Equal(t, 2, byID[backup.ID].BackupCredentialCount)
	require.Equal(t, 2, byID[backup.ID].CredentialReferenceCount)
	require.Equal(t, 1, byID[backup.ID].BackupProxyCount)
	require.Equal(t, 1, byID[backup.ID].ChannelReferenceCount)

	pools, err := svc.Pools(ctx)
	require.NoError(t, err)
	require.Equal(t, []OutboundProxyPoolDTO{{
		Name: "us-residential", ProxyCount: 2, EnabledProxyCount: 2, CredentialReferenceCount: 2,
	}}, pools)

	lowerLimit := 1
	_, err = svc.Update(ctx, backup.ID, OutboundProxyUpdateInput{MaxCredentials: &lowerLimit})
	require.ErrorContains(t, err, "current 2 credential references")

	_, err = svc.Update(ctx, primary.ID, OutboundProxyUpdateInput{BackupProxyID: &primary.ID})
	require.ErrorIs(t, err, ErrOutboundProxyBackupCycle)
	_, err = svc.Update(ctx, backup.ID, OutboundProxyUpdateInput{BackupProxyID: &primary.ID})
	require.ErrorIs(t, err, ErrOutboundProxyBackupCycle)

	require.ErrorIs(t, svc.Delete(ctx, backup.ID), ErrOutboundProxyReferenced)
	require.ErrorIs(t, svc.Delete(ctx, primary.ID), ErrOutboundProxyReferenced)
}

func TestOutboundProxyUpdateProjectsOnlyCredentialsUsingBackupFallback(t *testing.T) {
	svc, client, ctx := setupOutboundProxyService(t)
	defer client.Close()

	primaryLimit := 2
	backupLimit := 1
	primary, err := svc.Create(ctx, OutboundProxyCreateInput{
		Name: "primary", ProxyURL: "http://primary.example:8080", MaxCredentials: &primaryLimit,
	})
	require.NoError(t, err)
	backup, err := svc.Create(ctx, OutboundProxyCreateInput{
		Name: "backup", ProxyURL: "http://backup.example:8080", MaxCredentials: &backupLimit,
	})
	require.NoError(t, err)

	primaryID := primary.ID
	client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("fallback-projection-channel").
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-key"}).
		SetSupportedModels([]string{"m"}).
		SetDefaultTestModel("m").
		SetSettings(&objects.ChannelSettings{CredentialPolicies: []objects.ChannelCredentialPolicy{
			{ID: "key_backup", EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: &primaryID, EgressFailPolicy: objects.CredentialEgressFailPolicyBackupProxy},
			{ID: "key_closed", EgressMode: objects.CredentialEgressModePinnedProxy, OutboundProxyID: &primaryID, EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed},
		}}).
		SaveX(ctx)

	// The primary remains at two references; only key_backup consumes the new
	// backup's one-credential capacity. Counting every primary as a fallback
	// would incorrectly reject this change on either limit.
	updated, err := svc.Update(ctx, primary.ID, OutboundProxyUpdateInput{BackupProxyID: &backup.ID})
	require.NoError(t, err)
	require.Equal(t, 2, updated.CredentialReferenceCount)
	listed, err := svc.List(ctx)
	require.NoError(t, err)
	for _, item := range listed {
		if item.ID == backup.ID {
			require.Equal(t, 1, item.BackupCredentialCount)
			require.Equal(t, 1, item.CredentialReferenceCount)
		}
	}
}

func TestOutboundProxyProbeRequiresWriteScopeBeforeNetwork(t *testing.T) {
	var calls atomic.Int32
	svc := &OutboundProxyService{probeTransport: outboundProxyRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, nil
	})}
	readOnly := authz.NewUserContext(context.Background(), 123)
	readOnly = contexts.WithUser(readOnly, &ent.User{Scopes: []string{string(scopes.ScopeReadChannels)}})
	_, err := svc.ProbeOutboundProxy(readOnly, 1)
	require.ErrorContains(t, err, string(scopes.ScopeWriteChannels))
	require.Zero(t, calls.Load())
}

func TestOutboundProxyProbePersistsFallbackResultAndDoesNotFollowRedirect(t *testing.T) {
	svc, client, ctx := setupOutboundProxyService(t)
	defer client.Close()
	created, err := svc.Create(ctx, OutboundProxyCreateInput{Name: "probe", ProxyURL: "http://127.0.0.1:2260"})
	require.NoError(t, err)

	var targets []string
	svc.probeTransport = outboundProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		targets = append(targets, req.URL.String())
		if req.URL.Host == "cloudflare.com" {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://169.254.169.254/latest/meta-data"}},
				Body:       io.NopCloser(strings.NewReader("redirect")),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ip":"198.51.100.20"}`)),
			Request:    req,
		}, nil
	})

	probed, err := svc.ProbeOutboundProxy(ctx, created.ID)
	require.NoError(t, err)
	require.Equal(t, 2, len(targets))
	require.True(t, strings.HasPrefix(targets[0], "https://cloudflare.com/"))
	require.True(t, strings.HasPrefix(targets[1], "https://api.ipify.org"))
	require.True(t, *probed.LastTestSuccess)
	require.Equal(t, "198.51.100.20", probed.LastExitIP)
	require.NotNil(t, probed.LastTestedAt)
	require.Empty(t, probed.LastTestError)
}

func TestOutboundProxyProbeLimitsResponseBody(t *testing.T) {
	tooLarge := strings.Repeat("x", outboundProxyProbeMaxBody+1)
	svc := &OutboundProxyService{
		probeTransport: outboundProxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(tooLarge)),
				Request:    req,
			}, nil
		}),
	}

	result, err := svc.probeThroughProxy(t.Context(), "http://127.0.0.1:2260")
	require.NoError(t, err)
	require.False(t, result.success)
	require.Contains(t, result.errText, "too large")
}
