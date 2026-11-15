package biz

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestCompactAccountingOutbox_SequenceSealReplayConflictAndMigration(t *testing.T) {
	ctx := authz.WithTestBypass(t.Context())
	tempDir := t.TempDir()
	outboxPath := filepath.Join(tempDir, "node-b-outbox.db")

	cfg := cluster.CompactAccountingConfig{
		Enabled:       true,
		OutboxPath:    outboxPath,
		MaxBatchCount: 2,
		MaxBatchBytes: 256 << 10,
		MaxSpoolCount: 4,
		MaxSpoolBytes: 1 << 20,
	}

	outbox, err := OpenCompactAccountingOutbox(cfg, "b")
	require.NoError(t, err)
	producerUUID := outbox.ProducerUUID()
	require.NotEmpty(t, producerUUID)

	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	ev1, err := outbox.Append(ctx, CompactUsageEvent{
		EventUUID:  uuid.NewString(),
		CapturedAt: t0,
		ProjectID:  1,
		APIKeyID:   10,
		ChannelID:  20,
		ModelID:    "gpt-test",
		Source:     "api",
		Format:     "openai/responses",
		Usage:      llm.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		TotalCost:  nil, // Explicit null cost when model price is unconfigured.
	})
	require.NoError(t, err)
	require.Zero(t, ev1.Seq, "sequence is assigned by the background commit")

	ev2, err := outbox.Append(ctx, CompactUsageEvent{
		EventUUID:  uuid.NewString(),
		CapturedAt: t0.Add(5 * time.Second),
		ProjectID:  1,
		APIKeyID:   10,
		ChannelID:  20,
		ModelID:    "gpt-test",
		Source:     "api",
		Format:     "openai/responses",
		Usage:      llm.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
		TotalCost:  lo.ToPtr(0.0125),
	})
	require.NoError(t, err)
	require.Zero(t, ev2.Seq)

	ev3, err := outbox.Append(ctx, CompactUsageEvent{
		EventUUID:  uuid.NewString(),
		CapturedAt: t0.Add(10 * time.Second),
		ProjectID:  1,
		APIKeyID:   10,
		ChannelID:  20,
		ModelID:    "gpt-test",
		Source:     "api",
		Format:     "openai/responses",
		Usage:      llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5},
		TotalCost:  lo.ToPtr(0.005),
	})
	require.NoError(t, err)
	require.Zero(t, ev3.Seq)

	// Seal first batch [1,2].
	batch1, raw1, err := outbox.SealOrLoadNextBatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, batch1)
	require.Equal(t, int64(1), batch1.SeqStart)
	require.Equal(t, int64(2), batch1.SeqEnd)
	require.Len(t, batch1.Events, 2)
	require.Equal(t, ev1.EventUUID, batch1.Events[0].EventUUID)
	require.Equal(t, ev2.EventUUID, batch1.Events[1].EventUUID)
	ev2.Seq, ev3.Seq = 2, 3

	// Simulate crash before ACK: close and reopen SQLite outbox.
	require.NoError(t, outbox.Close(context.Background()))

	reopened, err := OpenCompactAccountingOutbox(cfg, "b")
	require.NoError(t, err)
	require.Equal(t, producerUUID, reopened.ProducerUUID())

	replayedBatch, replayedRaw, err := reopened.SealOrLoadNextBatch(ctx)
	require.NoError(t, err)
	require.Equal(t, batch1.PayloadDigest, replayedBatch.PayloadDigest)
	require.Equal(t, raw1, replayedRaw, "crash replay must emit the exact sealed batch bytes & digest")

	// Set up control-node SQLite Ent ledger to verify idempotent ingestion and conflict rejection.
	ledgerClient := enttest.NewEntClient(t, "sqlite3", "file:compact-ledger-unit?mode=memory&_fk=0")
	defer ledgerClient.Close()
	ledgerCtx := ent.NewContext(ctx, ledgerClient)

	proj := ledgerClient.Project.Create().SetName("p1").SetStatus(project.StatusActive).SaveX(ledgerCtx)
	usr := ledgerClient.User.Create().SetEmail("u@example.test").SetPassword("pw").SetStatus(user.StatusActivated).SaveX(ledgerCtx)
	keyEntity := ledgerClient.APIKey.Create().SetName("k1").SetKey("ah-unit").SetType(apikey.TypeUser).SetUserID(usr.ID).SetProjectID(proj.ID).SaveX(ledgerCtx)
	chEntity := ledgerClient.Channel.Create().SetName("c1").SetType(channel.TypeOpenaiResponses).SetBaseURL("https://example.test").SetCredentials(objects.ChannelCredentials{APIKey: "sk"}).SetSupportedModels([]string{"gpt-test"}).SetDefaultTestModel("gpt-test").SaveX(ledgerCtx)

	sysSvc := NewSystemService(SystemServiceParams{Ent: ledgerClient, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	chSvc := NewChannelService(ChannelServiceParams{Ent: ledgerClient, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, SystemService: sysSvc, HttpClient: httpclient.NewHttpClient()})
	defer chSvc.Stop()
	usageSvc := NewUsageLogService(ledgerClient, sysSvc, chSvc)
	var onCreatedCalls int
	usageSvc.OnUsageLogCreated = func() { onCreatedCalls++ }

	receiver := NewCompactAccountingService(CompactAccountingServiceParams{
		Ent:           ledgerClient,
		SystemService: sysSvc,
		UsageLog:      usageSvc,
		Node:          cluster.Config{Enabled: true, NodeID: "a", ControlNode: "a", Nodes: map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.1:2"}},
	})

	// Rewrite event IDs' project/key/channel to match created rows? Actually UsageLog foreign keys are disabled,
	// and we can check exact values stored!
	_ = keyEntity
	_ = chEntity

	ack1, err := receiver.IngestBatch(ledgerCtx, "b", *replayedBatch)
	require.NoError(t, err)
	require.False(t, ack1.Idempotent)
	require.Equal(t, 1, onCreatedCalls)

	// Lost ACK replay of the exact same sealed batch must succeed idempotently without double-counting.
	ackDup, err := receiver.IngestBatch(ledgerCtx, "b", *replayedBatch)
	require.NoError(t, err)
	require.True(t, ackDup.Idempotent)
	require.Equal(t, 1, onCreatedCalls)
	require.Equal(t, 2, ledgerClient.UsageLog.Query().CountX(ledgerCtx))

	// Tampered batch on the same (producer_uuid, seq_start, seq_end) with a different digest must be rejected with ErrCompactBatchConflict.
	tampered := *replayedBatch
	tampered.Events = append([]CompactUsageEvent(nil), replayedBatch.Events...)
	tampered.Events[0].Usage.TotalTokens = 999
	tamperedDigest, err := ComputeCompactBatchDigest(tampered.SchemaVersion, tampered.ProducerUUID, tampered.SeqStart, tampered.SeqEnd, tampered.Events)
	require.NoError(t, err)
	tampered.PayloadDigest = tamperedDigest
	_, err = receiver.IngestBatch(ledgerCtx, "b", tampered)
	require.ErrorIs(t, err, ErrCompactBatchConflict)

	// Overlapping sequence range [2,3] must also be rejected with ErrCompactBatchConflict.
	overlapEvents := []CompactUsageEvent{ev2, ev3}
	overlapDigest, err := ComputeCompactBatchDigest(CompactAccountingSchemaVersion, producerUUID, 2, 3, overlapEvents)
	require.NoError(t, err)
	_, err = receiver.IngestBatch(ledgerCtx, "b", CompactUsageBatchPayload{
		SchemaVersion: CompactAccountingSchemaVersion,
		ProducerUUID:  producerUUID,
		SeqStart:      2,
		SeqEnd:        3,
		PayloadDigest: overlapDigest,
		Events:        overlapEvents,
	})
	require.ErrorIs(t, err, ErrCompactBatchConflict)

	// Acknowledge [1,2] locally so only seq=3 remains unacked, then seal [3,3] and freeze/export for host migration.
	require.NoError(t, reopened.AckBatch(ctx, ack1.SeqStart, ack1.SeqEnd, ack1.PayloadDigest))
	stAfterAck, err := reopened.Status(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stAfterAck.UnackedCount)
	require.Equal(t, int64(2), stAfterAck.CommittedThroughSeq)

	sealed3, _, err := reopened.SealOrLoadNextBatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, sealed3)
	require.Equal(t, int64(3), sealed3.SeqStart)
	require.Equal(t, int64(3), sealed3.SeqEnd)

	exportDir := filepath.Join(tempDir, "frozen-export")
	manifest, err := reopened.FreezeAndExport(ctx, exportDir)
	require.NoError(t, err)
	require.Equal(t, producerUUID, manifest.ProducerUUID)
	require.Equal(t, 1, manifest.UnackedCount)
	require.True(t, manifest.HasSealedBatch)

	// Frozen outbox rejects new reservations before upstream execution.
	_, err = reopened.Reserve()
	require.ErrorIs(t, err, ErrCompactOutboxFrozen)
	require.NoError(t, reopened.Close(context.Background()))

	// Tampered export file must fail SHA-256 verification on import.
	corruptDir := filepath.Join(tempDir, "corrupt-export")
	require.NoError(t, os.MkdirAll(corruptDir, 0o750))
	rawManifest, err := os.ReadFile(filepath.Join(exportDir, compactExportManifestFilename))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(corruptDir, compactExportManifestFilename), rawManifest, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(corruptDir, compactExportSnapshotFilename), []byte("corrupted-bytes"), 0o600))
	_, err = ImportFrozenCompactOutbox(ctx, corruptDir, filepath.Join(tempDir, "should-not-exist.db"))
	require.ErrorContains(t, err, "snapshot integrity mismatch")

	// Valid import onto replacement host path preserves producer_uuid, event_uuid, seq=3, and sealed batch, and unfreezes.
	importedPath := filepath.Join(tempDir, "replacement-node-c-outbox.db")
	importStatus, err := ImportFrozenCompactOutbox(ctx, exportDir, importedPath)
	require.NoError(t, err)
	require.False(t, importStatus.Frozen)
	require.Equal(t, producerUUID, importStatus.ProducerUUID)
	require.Equal(t, 1, importStatus.UnackedCount)

	migratedOutbox, err := OpenCompactAccountingOutbox(cluster.CompactAccountingConfig{
		Enabled:       true,
		OutboxPath:    importedPath,
		MaxSpoolCount: 2,
		MaxSpoolBytes: 1 << 20,
	}, "c")
	require.NoError(t, err)
	defer migratedOutbox.Close(context.Background())

	migratedBatch, _, err := migratedOutbox.SealOrLoadNextBatch(ctx)
	require.NoError(t, err)
	require.Equal(t, sealed3.PayloadDigest, migratedBatch.PayloadDigest)
	require.Equal(t, ev3.EventUUID, migratedBatch.Events[0].EventUUID)

	// Ingest migrated batch on control node under new sender node ID "c" — digest remains valid and commits cleanly.
	ack3, err := receiver.IngestBatch(ledgerCtx, "c", *migratedBatch)
	require.NoError(t, err)
	require.False(t, ack3.Idempotent)
	require.NoError(t, migratedOutbox.AckBatch(ctx, ack3.SeqStart, ack3.SeqEnd, ack3.PayloadDigest))

	// Verify all 3 events landed in usage_logs with original captured_at and null vs non-null total_cost preserved.
	rows := ledgerClient.UsageLog.Query().Order(ent.Asc("id")).AllX(ledgerCtx)
	require.Len(t, rows, 3)
	require.Equal(t, 0, rows[0].RequestID)
	require.Equal(t, ev1.EventUUID, *rows[0].EventID)
	require.Nil(t, rows[0].TotalCost)
	require.True(t, rows[0].CreatedAt.Equal(t0))
	require.NotNil(t, rows[1].TotalCost)
	require.InDelta(t, 0.0125, *rows[1].TotalCost, 1e-9)
	require.True(t, rows[2].CreatedAt.Equal(t0.Add(10*time.Second)))
}

func TestCompactAccountingAdmission_SpoolFullAndPostExecutionDrop(t *testing.T) {
	ctx := authz.WithTestBypass(t.Context())
	client := enttest.NewEntClient(t, "sqlite3", "file:compact-admission-fence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	proj := client.Project.Create().SetName("p").SetStatus(project.StatusActive).SaveX(ctx)
	usr := client.User.Create().SetEmail("owner@example.test").SetPassword("pw").SetStatus(user.StatusActivated).SaveX(ctx)
	nonQuotaKey := client.APIKey.Create().
		SetName("non-quota").
		SetKey("ah-non-quota").
		SetType(apikey.TypeUser).
		SetUserID(usr.ID).
		SetProjectID(proj.ID).
		SaveX(ctx)

	outboxPath := filepath.Join(t.TempDir(), "spool-bound.db")
	nodeCfg := cluster.Config{
		Enabled:     true,
		NodeID:      "b",
		ControlNode: "a",
		Nodes:       map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.1:2"},
		CompactAccounting: cluster.CompactAccountingConfig{
			Enabled:       true,
			OutboxPath:    outboxPath,
			SyncInterval:  time.Hour,
			MaxSpoolCount: 1,
			MaxSpoolBytes: 1 << 20,
		},
	}

	sysSvc := NewSystemService(SystemServiceParams{Ent: client, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	chSvc := NewChannelService(ChannelServiceParams{Ent: client, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, SystemService: sysSvc, HttpClient: httpclient.NewHttpClient(), Node: nodeCfg})
	defer chSvc.Stop()
	storageSvc := NewDataStorageService(DataStorageServiceParams{Client: client, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, SystemService: sysSvc})
	usageSvc := NewUsageLogService(client, sysSvc, chSvc)
	writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{})
	reqSvc := NewRequestServiceWithObservationWriter(client, sysSvc, usageSvc, storageSvc, NewLiveStreamRegistry(), nil, writer)
	compactSvc := NewCompactAccountingService(CompactAccountingServiceParams{
		Ent:           client,
		SystemService: sysSvc,
		UsageLog:      usageSvc,
		Node:          nodeCfg,
	})
	writer.SetCompactAccountingService(compactSvc)

	require.NoError(t, writer.Start(ctx))
	defer func() { _ = writer.Stop(context.Background()) }()
	require.NoError(t, compactSvc.Start(ctx))
	defer func() { _ = compactSvc.Stop(context.Background()) }()

	makeReqCtx := func() context.Context {
		c := contexts.WithProjectID(ctx, proj.ID)
		c = contexts.WithSource(c, request.SourceAPI)
		c = contexts.WithAPIKey(c, nonQuotaKey)
		return reqSvc.ForwardingObservationContext(c)
	}

	// A full accounting spool degrades accounting, never inference admission.
	ctx1 := makeReqCtx()
	defer EndForwardingObservation(ctx1)
	synthReq1, err := reqSvc.CreateRequest(ctx1, &llm.Request{Model: "m"}, &httpclient.Request{Body: []byte(`{}`)}, llm.APIFormatOpenAIResponse)
	require.NoError(t, err)
	require.Less(t, synthReq1.ID, 0)

	ctx2 := makeReqCtx()
	defer EndForwardingObservation(ctx2)
	req2, err := reqSvc.CreateRequest(ctx2, &llm.Request{Model: "m"}, &httpclient.Request{Body: []byte(`{}`)}, llm.APIFormatOpenAIResponse)
	require.NoError(t, err)
	require.Less(t, req2.ID, 0)
	require.ErrorIs(t, observationFromContext(ctx2).compactDegraded, ErrCompactOutboxSpoolFull)
	status, err := compactSvc.Outbox().Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.AdmissionFailures)

	// A post-execution SQLite outage must preserve the completed upstream response.
	exec1, err := reqSvc.CreateRequestExecution(ctx1, &Channel{Channel: &ent.Channel{ID: 7}}, "m", synthReq1, httpclient.Request{}, llm.APIFormatOpenAIResponse, false)
	require.NoError(t, err)
	require.NoError(t, compactSvc.Outbox().Close(context.Background()))

	_, usageErr := usageSvc.CreateUsageLogFromRequest(ctx1, synthReq1, exec1, &llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5})
	require.NoError(t, usageErr)
	require.NoError(t, LastForwardingAccountingError(ctx1))
	status, err = compactSvc.Outbox().Status(ctx)
	require.ErrorIs(t, err, ErrCompactOutboxUnavailable)
	require.EqualValues(t, 1, status.DroppedEvents)
	require.Zero(t, status.ReservedCount)
	// Duplicate submission does not count the dropped fact twice.
	_, usageErr = usageSvc.CreateUsageLogFromRequest(ctx1, synthReq1, exec1, &llm.Usage{TotalTokens: 5})
	require.NoError(t, usageErr)
	status, _ = compactSvc.Outbox().Status(ctx)
	require.EqualValues(t, 1, status.DroppedEvents)

	// Zero rows written to remote Request, RequestExecution, or PendingUsage tables.
	require.Zero(t, client.Request.Query().CountX(ctx))
	require.Zero(t, client.RequestExecution.Query().CountX(ctx))
	require.Zero(t, client.PendingUsage.Query().CountX(ctx))
}

func TestSystemService_LocalSnapshotDeepCloneOnSQLite(t *testing.T) {
	ctx := authz.WithTestBypass(t.Context())
	client := enttest.NewEntClient(t, "sqlite3", "file:system-snapshot-sqlite?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	sysSvc := NewSystemService(SystemServiceParams{
		Ent:         client,
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
	})
	require.NoError(t, sysSvc.Start(ctx))
	defer sysSvc.Stop()

	require.NoError(t, sysSvc.SetRetryPolicy(ctx, &RetryPolicy{
		Enabled:                   true,
		MaxChannelRetries:         3,
		EmptyResponseTextPatterns: []string{"placeholder"},
		AutoDisableChannel: AutoDisableChannel{
			Enabled:  true,
			Statuses: []AutoDisableChannelStatus{{Status: 401, Times: 2}},
		},
		UpstreamErrorPolicy: UpstreamErrorPolicy{
			Mode:          UpstreamErrorModeCustom,
			CustomMessage: "masked error",
		},
	}))

	read1, err := sysSvc.RetryPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, read1.MaxChannelRetries)
	require.Equal(t, "masked error", read1.UpstreamErrorPolicy.CustomMessage)

	// Mutating returned slice or struct fields in a caller must never corrupt the in-process atomic snapshot.
	read1.MaxChannelRetries = 99
	read1.EmptyResponseTextPatterns[0] = "corrupted"
	read1.AutoDisableChannel.Statuses[0].Times = 999

	read2, err := sysSvc.RetryPolicy(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, read2.MaxChannelRetries)
	require.Equal(t, []string{"placeholder"}, read2.EmptyResponseTextPatterns)
	require.Equal(t, 2, read2.AutoDisableChannel.Statuses[0].Times)
}
