package gc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/managedobservabilitystate"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	serverdb "github.com/looplj/axonhub/internal/server/db"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

const postgresManagedIntegrationEnv = "AXONHUB_TEST_PG_DSN"

type postgresManagedFixture struct {
	client   *ent.Client
	ctx      context.Context
	system   *biz.SystemService
	requests *biz.RequestService
	usage    *biz.UsageLogService
	storage  *biz.DataStorageService
	worker   *Worker
	project  *ent.Project
	channel  *biz.Channel
}

func newPostgresManagedFixture(t *testing.T, dsn string) *postgresManagedFixture {
	t.Helper()
	client := serverdb.NewEntClient(serverdb.Config{
		Dialect:      "postgres",
		DSN:          dsn,
		MaxOpenConns: 8,
		MaxIdleConns: 4,
	})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	channelService := biz.NewChannelServiceForTest(client)
	usage := biz.NewUsageLogService(client, system, channelService)
	storage := biz.NewDataStorageService(biz.DataStorageServiceParams{
		SystemService: system,
		CacheConfig:   xcache.Config{},
		Client:        client,
	})
	requests := biz.NewRequestService(client, system, usage, storage, biz.NewLiveStreamRegistry())
	proj := client.Project.Create().SetName("managed-pg-contract").SetStatus(project.StatusActive).SaveX(ctx)
	client.DataStorage.Create().
		SetName("managed-pg-primary").
		SetDescription("disposable managed observability integration database").
		SetPrimary(true).
		SetType(datastorage.TypeDatabase).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		SaveX(ctx)
	ch := client.Channel.Create().
		SetType(entchannel.TypeAxonhub).
		SetName("managed-pg-channel").
		SetBaseURL("http://upstream.invalid").
		SetCredentials(objects.ChannelCredentials{}).
		SetSupportedModels([]string{"gpt-4o"}).
		SetManualModels([]string{}).
		SetDefaultTestModel("gpt-4o").
		SetSettings(&objects.ChannelSettings{}).
		SaveX(ctx)
	return &postgresManagedFixture{
		client: client, ctx: contexts.WithProjectID(ctx, proj.ID), system: system,
		requests: requests, usage: usage, storage: storage, project: proj,
		channel: &biz.Channel{Channel: ch},
		worker:  &Worker{Ent: client, SystemService: system, DataStorageService: storage},
	}
}

func randomJSONBody(t *testing.T, targetBytes int) []byte {
	t.Helper()
	raw := make([]byte, targetBytes)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return []byte(fmt.Sprintf(`{"data":"%s"}`, base64.StdEncoding.EncodeToString(raw)))
}

func (f *postgresManagedFixture) createRequest(t *testing.T, body []byte) *ent.Request {
	t.Helper()
	row, err := f.requests.CreateRequest(f.ctx, &llm.Request{Model: "gpt-4o"},
		&httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	return row
}

func (f *postgresManagedFixture) cleanupOwned(t *testing.T, policy *biz.StoragePolicy) {
	t.Helper()
	var cleanupErr error
	acquired, err := f.worker.withGCOwnership(f.ctx, func() {
		cleanupErr = f.worker.cleanupManagedCapacity(f.ctx, policy)
	})
	require.NoError(t, err)
	require.True(t, acquired)
	require.NoError(t, cleanupErr)
}

// TestManagedObservabilityPostgresApplicationContract is intentionally guarded
// twice: the DSN must be supplied and the caller must attest it is a disposable
// database. scripts/postgres-managed-observability-integration.sh is the normal
// entry point and never accepts an operator DSN.
func TestManagedObservabilityPostgresApplicationContract(t *testing.T) {
	dsn := os.Getenv(postgresManagedIntegrationEnv)
	if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires a disposable PostgreSQL DSN and AXONHUB_TEST_PG_DISPOSABLE=1")
	}
	f := newPostgresManagedFixture(t, dsn)
	require.NoError(t, f.system.SetStoragePolicy(f.ctx, &biz.StoragePolicy{
		StoreChunks: true, StoreRequestBody: true, StoreExecutionRequestBody: lo.ToPtr(true), StoreResponseBody: true,
	}))

	rawDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rawDB.Close()) })
	var fkCount int
	require.NoError(t, rawDB.QueryRowContext(f.ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE contype = 'f' AND conrelid IN ('observability_payloads'::regclass, 'requests'::regclass, 'request_executions'::regclass)
	`).Scan(&fkCount))
	require.Zero(t, fkCount, "the harness must exercise the runtime WithForeignKeys(false) schema")

	t.Run("runtime migration exact dedup and application lifecycle", func(t *testing.T) {
		body := randomJSONBody(t, 576<<10)
		variant := append([]byte(nil), body...)
		variant[len(variant)-3] ^= 1
		parent := f.createRequest(t, body)
		first, err := f.requests.CreateRequestExecution(f.ctx, f.channel, "gpt-4o", parent,
			httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
		require.NoError(t, err)
		second, err := f.requests.CreateRequestExecution(f.ctx, f.channel, "gpt-4o", parent,
			httpclient.Request{JSONBody: variant}, llm.APIFormatOpenAIChatCompletion, true)
		require.NoError(t, err)
		require.Equal(t, parent.RequestBodyPayloadID, first.RequestBodyPayloadID)
		require.NotEqual(t, first.RequestBodyPayloadID, second.RequestBodyPayloadID)
		require.Equal(t, 2, f.client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(parent.ID)).CountX(f.ctx))

		f.client.Request.UpdateOneID(parent.ID).SetStatus(request.StatusCompleted).SaveX(f.ctx)
		f.client.RequestExecution.UpdateOneID(first.ID).SetStatus(requestexecution.StatusCompleted).SaveX(f.ctx)
		f.client.RequestExecution.UpdateOneID(second.ID).SetStatus(requestexecution.StatusCompleted).SaveX(f.ctx)
		cutoff := time.Now().UTC().Add(time.Hour)
		deleted, err := f.worker.deleteExecutionCandidate(f.ctx, first.ID, cutoff, map[int]*ent.DataStorage{})
		require.NoError(t, err)
		require.True(t, deleted)
		deleted, err = f.worker.deleteExecutionCandidate(f.ctx, second.ID, cutoff, map[int]*ent.DataStorage{})
		require.NoError(t, err)
		require.True(t, deleted)
		deleted, err = f.worker.deleteRequestCandidate(f.ctx, parent.ID, cutoff, map[int]*ent.DataStorage{})
		require.NoError(t, err)
		require.True(t, deleted)
		require.Zero(t, f.client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(parent.ID)).CountX(f.ctx))
		require.Zero(t, f.client.ManagedObservabilityState.GetX(f.ctx, 1).ChargedBytes)

		legacy := f.client.Request.Create().SetProjectID(f.project.ID).SetModelID("legacy").
			SetRequestBody([]byte(`{"legacy":true}`)).SetStatus(request.StatusCompleted).SaveX(f.ctx)
		deleted, err = f.worker.deleteRequestCandidate(f.ctx, legacy.ID, cutoff, map[int]*ent.DataStorage{})
		require.NoError(t, err)
		require.True(t, deleted)
	})

	t.Run("priority active exclusion and failed diagnostic retention", func(t *testing.T) {
		body := randomJSONBody(t, 552<<10)
		activeBody := append(append([]byte(nil), body...), '\n')
		low := f.createRequest(t, body)
		failed := f.createRequest(t, append(append([]byte(nil), body...), ' '))
		active := f.createRequest(t, activeBody)
		f.client.Request.UpdateOneID(low.ID).SetStatus(request.StatusCompleted).SaveX(f.ctx)
		f.client.Request.UpdateOneID(failed.ID).SetStatus(request.StatusFailed).SaveX(f.ctx)
		f.client.Request.UpdateOneID(active.ID).SetStatus(request.StatusCompleted).SaveX(f.ctx)
		activeExecution, err := f.requests.CreateRequestExecution(f.ctx, f.channel, "gpt-4o", active,
			httpclient.Request{JSONBody: activeBody}, llm.APIFormatOpenAIChatCompletion, false)
		require.NoError(t, err)

		policy := &biz.StoragePolicy{
			StoreChunks: true, StoreRequestBody: true, StoreExecutionRequestBody: lo.ToPtr(true), StoreResponseBody: true,
			ManagedObservabilityHardMiB: lo.ToPtr(4), ManagedObservabilityLowMiB: lo.ToPtr(3),
		}
		require.NoError(t, f.system.SetStoragePolicy(f.ctx, policy))
		f.cleanupOwned(t, policy)
		low = f.client.Request.GetX(f.ctx, low.ID)
		failed = f.client.Request.GetX(f.ctx, failed.ID)
		active = f.client.Request.GetX(f.ctx, active.ID)
		require.Nil(t, low.RequestBodyPayloadID, "successful evidence is evicted before failed evidence")
		require.NotNil(t, failed.RequestBodyPayloadID)
		require.NotNil(t, active.RequestBodyPayloadID, "terminal parent with a processing child is excluded")
		loaded, err := f.requests.LoadRequestBodyBounded(f.ctx, failed, 2<<20)
		require.NoError(t, err)
		require.NotEmpty(t, loaded, "failed diagnostic evidence remains readable")
		state := f.client.ManagedObservabilityState.GetX(f.ctx, 1)
		require.False(t, state.UnderPressure)
		require.LessOrEqual(t, state.ChargedBytes, int64(3<<20))

		f.client.RequestExecution.UpdateOneID(activeExecution.ID).SetStatus(requestexecution.StatusCompleted).SaveX(f.ctx)
		cutoff := time.Now().UTC().Add(time.Hour)
		deleted, err := f.worker.deleteExecutionCandidate(f.ctx, activeExecution.ID, cutoff, map[int]*ent.DataStorage{})
		require.NoError(t, err)
		require.True(t, deleted)
		for _, id := range []int{low.ID, failed.ID, active.ID} {
			deleted, err = f.worker.deleteRequestCandidate(f.ctx, id, cutoff, map[int]*ent.DataStorage{})
			require.NoError(t, err)
			require.True(t, deleted)
		}
		require.Zero(t, f.client.ObservabilityPayload.Query().CountX(f.ctx))
	})

	t.Run("repeated hard low application cycles remain fail open", func(t *testing.T) {
		policy := &biz.StoragePolicy{
			StoreChunks: true, StoreRequestBody: false, StoreExecutionRequestBody: lo.ToPtr(false), StoreResponseBody: true,
			ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1),
		}
		require.NoError(t, f.system.SetStoragePolicy(f.ctx, policy))
		f.cleanupOwned(t, policy)
		for cycle := 1; cycle <= 8; cycle++ {
			first := f.createRequest(t, []byte(`{"omitted":true}`))
			require.NoError(t, f.requests.UpdateRequestCompleted(f.ctx, first.ID, fmt.Sprintf("first-%d", cycle),
				map[string]any{"data": strings.Repeat("r", 900<<10)}, nil))
			chunkJSON := []byte(`"` + strings.Repeat("c", 600<<10) + `"`)
			require.NoError(t, f.requests.SaveRequestChunks(f.ctx, first.ID, []*httpclient.StreamEvent{{Type: "data", Data: chunkJSON}}))
			first = f.client.Request.GetX(f.ctx, first.ID)
			require.Empty(t, first.ResponseChunks)
			require.Contains(t, *first.EvidenceDisposition.ResponseChunks.FailureClass, "capacity_pressure")

			second := f.createRequest(t, []byte(`{"omitted":true}`))
			require.NoError(t, f.requests.UpdateRequestCompleted(f.ctx, second.ID, fmt.Sprintf("second-%d", cycle),
				map[string]any{"data": strings.Repeat("s", 900<<10)}, nil), "pressure must not fail the request completion seam")
			second = f.client.Request.GetX(f.ctx, second.ID)
			require.Equal(t, request.StatusCompleted, second.Status)
			require.Empty(t, second.ResponseBody)
			require.Contains(t, *second.EvidenceDisposition.ResponseBody.FailureClass, "capacity_pressure")
			usage, err := f.usage.CreateUsageLog(f.ctx, biz.CreateUsageLogParams{
				RequestID: second.ID, ProjectID: second.ProjectID, ChannelID: f.channel.ID,
				ActualModelID: "gpt-4o", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
				Source: usagelog.SourceAPI, Format: string(llm.APIFormatOpenAIChatCompletion),
			})
			require.NoError(t, err)
			require.NotNil(t, usage)
			require.Equal(t, int64(30), usage.TotalTokens)
			require.True(t, f.client.ManagedObservabilityState.GetX(f.ctx, 1).UnderPressure)

			f.cleanupOwned(t, policy)
			state := f.client.ManagedObservabilityState.GetX(f.ctx, 1)
			require.False(t, state.UnderPressure)
			require.LessOrEqual(t, state.ChargedBytes, int64(1<<20))
			t.Logf("application cycle=%d charged_bytes=%d requests=%d payloads=%d", cycle, state.ChargedBytes,
				f.client.Request.Query().CountX(f.ctx), f.client.ObservabilityPayload.Query().CountX(f.ctx))
		}
	})

	t.Run("two clients contend and connection loss releases actual owner", func(t *testing.T) {
		secondClient := serverdb.NewEntClient(serverdb.Config{
			Dialect: "postgres", DSN: dsn, MaxOpenConns: 4, MaxIdleConns: 2, DisableAutoMigration: true,
		})
		defer secondClient.Close()
		secondCtx := authz.WithTestBypass(ent.NewContext(context.Background(), secondClient))
		secondSystem := biz.NewSystemService(biz.SystemServiceParams{Ent: secondClient})
		secondWorker := &Worker{Ent: secondClient, SystemService: secondSystem}

		entered := make(chan struct{})
		release := make(chan struct{})
		result := make(chan error, 1)
		var once sync.Once
		go func() {
			acquired, lockErr := f.worker.withGCOwnership(f.ctx, func() {
				once.Do(func() { close(entered) })
				<-release
			})
			if lockErr == nil && !acquired {
				lockErr = fmt.Errorf("first client did not acquire owner lock")
			}
			result <- lockErr
		}()
		<-entered
		acquired, err := secondWorker.withGCOwnership(secondCtx, func() {})
		require.NoError(t, err)
		require.False(t, acquired)

		var ownerPID int
		require.Eventually(t, func() bool {
			return rawDB.QueryRowContext(f.ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted ORDER BY pid LIMIT 1`).Scan(&ownerPID) == nil
		}, 5*time.Second, 50*time.Millisecond)
		var terminated bool
		require.NoError(t, rawDB.QueryRowContext(f.ctx, `SELECT pg_terminate_backend($1)`, ownerPID).Scan(&terminated))
		require.True(t, terminated)
		require.Eventually(t, func() bool {
			owned, ownerErr := secondWorker.withGCOwnership(secondCtx, func() {})
			return ownerErr == nil && owned
		}, 5*time.Second, 100*time.Millisecond)
		close(release)
		require.NoError(t, <-result)
	})

	var payloadRelation, requestRelation, executionRelation, stateRelation int64
	require.NoError(t, rawDB.QueryRowContext(f.ctx, `SELECT
		pg_total_relation_size('observability_payloads'),
		pg_total_relation_size('requests'),
		pg_total_relation_size('request_executions'),
		pg_total_relation_size('managed_observability_states')
	`).Scan(&payloadRelation, &requestRelation, &executionRelation, &stateRelation))
	state, err := f.client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1)).Only(f.ctx)
	require.NoError(t, err)
	t.Logf("application_relations payload=%d request=%d execution=%d state=%d charged=%d under_pressure=%v last_error=%q",
		payloadRelation, requestRelation, executionRelation, stateRelation, state.ChargedBytes, state.UnderPressure, state.LastError)
}
