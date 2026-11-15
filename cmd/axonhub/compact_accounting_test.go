package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm"
)

func TestCompactOutboxCLIHelpers_ExportAndImport(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	srcPath := filepath.Join(tempDir, "src-outbox.db")
	exportDir := filepath.Join(tempDir, "exported-bundle")
	dstPath := filepath.Join(tempDir, "dst-outbox.db")

	outbox, err := biz.OpenCompactAccountingOutbox(cluster.CompactAccountingConfig{
		Enabled:    true,
		OutboxPath: srcPath,
	}, "node-b")
	require.NoError(t, err)

	origEvent, err := outbox.Append(ctx, biz.CompactUsageEvent{
		EventUUID:  uuid.NewString(),
		CapturedAt: time.Now().UTC(),
		ProjectID:  1,
		APIKeyID:   2,
		ChannelID:  3,
		ModelID:    "m1",
		Source:     "api",
		Format:     "openai/responses",
		Usage:      llm.Usage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10},
	})
	require.NoError(t, err)
	producerUUID := outbox.ProducerUUID()
	require.NoError(t, outbox.Close(context.Background()))

	manifest, err := biz.ExportFrozenCompactOutbox(ctx, srcPath, exportDir)
	require.NoError(t, err)
	require.Equal(t, producerUUID, manifest.ProducerUUID)
	require.Equal(t, 1, manifest.UnackedCount)

	srcStatus, err := biz.InspectCompactOutbox(ctx, srcPath)
	require.NoError(t, err)
	require.True(t, srcStatus.Frozen)

	dstStatus, err := biz.ImportFrozenCompactOutbox(ctx, exportDir, dstPath)
	require.NoError(t, err)
	require.False(t, dstStatus.Frozen)
	require.Equal(t, producerUUID, dstStatus.ProducerUUID)
	require.Equal(t, 1, dstStatus.UnackedCount)

	// Refuses overwriting a non-empty target outbox.
	_, err = biz.ImportFrozenCompactOutbox(ctx, exportDir, dstPath)
	require.ErrorContains(t, err, "already has 1 unacked events")

	dstOutbox, err := biz.OpenCompactAccountingOutbox(cluster.CompactAccountingConfig{
		Enabled:    true,
		OutboxPath: dstPath,
	}, "node-c")
	require.NoError(t, err)
	defer dstOutbox.Close(context.Background())

	batch, _, err := dstOutbox.SealOrLoadNextBatch(ctx)
	require.NoError(t, err)
	require.Len(t, batch.Events, 1)
	require.Equal(t, origEvent.EventUUID, batch.Events[0].EventUUID)
	require.Equal(t, int64(1), batch.Events[0].Seq)
}
