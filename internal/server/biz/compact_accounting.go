package biz

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/internal/objects"
	_ "github.com/looplj/axonhub/internal/pkg/sqlite"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm"
)

const (
	CompactAccountingSchemaVersion = 1
	compactExportSnapshotFilename  = "outbox.sqlite"
	compactExportManifestFilename  = "manifest.json"
)

var (
	ErrCompactOutboxUnavailable = errors.New("compact accounting outbox unavailable")
	ErrCompactOutboxFrozen      = errors.New("compact accounting outbox is frozen for migration")
	ErrCompactOutboxSpoolFull   = errors.New("compact accounting outbox spool capacity exceeded")
	ErrCompactBatchConflict     = errors.New("compact accounting batch sequence or digest conflict")
)

// CompactUsageEvent represents one locally committed accounting fact ready for
// low-frequency batch delivery to the control node's central usage_logs ledger.
type CompactUsageEvent struct {
	Seq              int64              `json:"seq"`
	EventUUID        string             `json:"event_uuid"`
	CapturedAt       time.Time          `json:"captured_at"`
	ProjectID        int                `json:"project_id"`
	APIKeyID         int                `json:"api_key_id"`
	ChannelID        int                `json:"channel_id"`
	ModelID          string             `json:"model_id"`
	Source           string             `json:"source"`
	Format           string             `json:"format"`
	Usage            llm.Usage          `json:"usage"`
	TotalCost        *float64           `json:"total_cost"`
	CostItems        []objects.CostItem `json:"cost_items,omitempty"`
	PriceReferenceID string             `json:"price_reference_id,omitempty"`
}

// CompactUsageBatchPayload is the wire & digest payload for a sealed batch.
// NodeID is carried by the authenticated transport envelope rather than the
// canonical digest so a migrated frozen outbox replaying on a replacement node
// preserves the exact sealed batch digest.
type CompactUsageBatchPayload struct {
	SchemaVersion int                 `json:"schema_version"`
	ProducerUUID  string              `json:"producer_uuid"`
	SeqStart      int64               `json:"seq_start"`
	SeqEnd        int64               `json:"seq_end"`
	PayloadDigest string              `json:"payload_digest"`
	Events        []CompactUsageEvent `json:"events"`
}

type canonicalCompactBatchDigestInput struct {
	SchemaVersion int                 `json:"schema_version"`
	ProducerUUID  string              `json:"producer_uuid"`
	SeqStart      int64               `json:"seq_start"`
	SeqEnd        int64               `json:"seq_end"`
	Events        []CompactUsageEvent `json:"events"`
}

// ComputeCompactBatchDigest returns the deterministic SHA-256 hex digest over
// the batch's schema_version, producer_uuid, sequence range, and events.
func ComputeCompactBatchDigest(schemaVersion int, producerUUID string, seqStart, seqEnd int64, events []CompactUsageEvent) (string, error) {
	raw, err := json.Marshal(canonicalCompactBatchDigestInput{
		SchemaVersion: schemaVersion,
		ProducerUUID:  producerUUID,
		SeqStart:      seqStart,
		SeqEnd:        seqEnd,
		Events:        events,
	})
	if err != nil {
		return "", fmt.Errorf("marshal canonical compact batch: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// CompactOutboxStatus reports the local durable accounting outbox state.
type CompactOutboxStatus struct {
	Path                string    `json:"path"`
	ProducerUUID        string    `json:"producer_uuid"`
	Frozen              bool      `json:"frozen"`
	NextSeq             int64     `json:"next_seq"`
	CommittedThroughSeq int64     `json:"committed_through_seq"`
	UnackedCount        int       `json:"unacked_count"`
	UnackedBytes        int64     `json:"unacked_bytes"`
	ReservedCount       int       `json:"reserved_count"`
	QueuedCount         int       `json:"queued_count"`
	QueuedBytes         int64     `json:"queued_bytes"`
	AdmissionFailures   uint64    `json:"admission_failures"`
	DroppedEvents       uint64    `json:"dropped_events"`
	WriteErrors         uint64    `json:"write_errors"`
	LastError           string    `json:"last_error,omitempty"`
	HasSealedBatch      bool      `json:"has_sealed_batch"`
	SealedSeqStart      int64     `json:"sealed_seq_start,omitempty"`
	SealedSeqEnd        int64     `json:"sealed_seq_end,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// CompactOutboxExportManifest describes a frozen, single-file VACUUM INTO export
// of a node's compact accounting outbox for safe cross-host recovery.
type CompactOutboxExportManifest struct {
	SchemaVersion       int       `json:"schema_version"`
	SnapshotFile        string    `json:"snapshot_file"`
	SHA256              string    `json:"sha256"`
	ByteSize            int64     `json:"byte_size"`
	ProducerUUID        string    `json:"producer_uuid"`
	NextSeq             int64     `json:"next_seq"`
	CommittedThroughSeq int64     `json:"committed_through_seq"`
	UnackedCount        int       `json:"unacked_count"`
	UnackedBytes        int64     `json:"unacked_bytes"`
	HasSealedBatch      bool      `json:"has_sealed_batch"`
	SealedSeqStart      int64     `json:"sealed_seq_start,omitempty"`
	SealedSeqEnd        int64     `json:"sealed_seq_end,omitempty"`
	ExportedAt          time.Time `json:"exported_at"`
}

// CompactAccountingOutbox manages the local durable SQLite WAL outbox on a
// non-control data node.
type CompactAccountingOutbox struct {
	cfg  cluster.CompactAccountingConfig
	path string

	// mu protects only memory state; request admission never waits for SQLite.
	mu                sync.Mutex
	dbMu              sync.Mutex
	db                *sql.DB
	closed            bool
	producerUUID      string
	frozen            bool
	unackedCount      int
	unackedBytes      int64
	reservedCount     int
	pendingCount      int
	pendingBytes      int64
	admissionFailures uint64
	droppedEvents     uint64
	writeErrors       uint64
	lastError         string
	queue             chan compactQueuedEvent
	idle              chan struct{}
	done              chan struct{}
	closeErr          error
}

type compactQueuedEvent struct {
	event CompactUsageEvent
	bytes int64
}

const compactReservationBytes int64 = 2048

// OpenCompactAccountingOutbox opens or initializes a durable SQLite WAL outbox.
func OpenCompactAccountingOutbox(cfg cluster.CompactAccountingConfig, nodeID string) (*CompactAccountingOutbox, error) {
	cfg = cfg.WithDefaults(nodeID)
	cleanPath := filepath.Clean(cfg.OutboxPath)
	if dir := filepath.Dir(cleanPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create compact outbox directory %s: %w", dir, err)
		}
	}

	sqlDB, err := openCompactSQLite(cleanPath)
	if err != nil {
		return nil, err
	}

	outbox := &CompactAccountingOutbox{
		cfg:  cfg,
		path: cleanPath,
		db:   sqlDB,
	}
	if err := outbox.initSchemaAndState(context.Background()); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	outbox.queue = make(chan compactQueuedEvent, min(1024, cfg.MaxSpoolCount))
	outbox.idle = make(chan struct{})
	close(outbox.idle)
	outbox.done = make(chan struct{})
	go outbox.writeLoop()
	return outbox, nil
}

func openCompactSQLite(path string) (*sql.DB, error) {
	dsn := path
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"

	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open compact outbox sqlite %s: %w", path, err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping compact outbox sqlite %s: %w", path, err)
	}
	return sqlDB, nil
}

func (o *CompactAccountingOutbox) initSchemaAndState(ctx context.Context) error {
	const ddl = `
CREATE TABLE IF NOT EXISTS outbox_meta (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	schema_version INTEGER NOT NULL,
	producer_uuid TEXT NOT NULL,
	next_seq INTEGER NOT NULL,
	committed_through_seq INTEGER NOT NULL,
	frozen INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox_events (
	seq INTEGER PRIMARY KEY,
	event_uuid TEXT NOT NULL UNIQUE,
	captured_at INTEGER NOT NULL,
	payload_json BLOB NOT NULL,
	payload_bytes INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS outbox_sealed_batch (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq_start INTEGER NOT NULL,
	seq_end INTEGER NOT NULL,
	event_count INTEGER NOT NULL,
	payload_json BLOB NOT NULL,
	payload_digest TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`
	if _, err := o.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("initialize compact outbox schema: %w", err)
	}

	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin compact outbox init tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var producerUUID string
	var frozenInt int
	err = tx.QueryRowContext(ctx, `SELECT producer_uuid, frozen FROM outbox_meta WHERE id = 1`).Scan(&producerUUID, &frozenInt)
	if errors.Is(err, sql.ErrNoRows) {
		producerUUID = uuid.NewString()
		now := time.Now().UTC().UnixNano()
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO outbox_meta (id, schema_version, producer_uuid, next_seq, committed_through_seq, frozen, updated_at) VALUES (1, ?, ?, 1, 0, 0, ?)`,
			CompactAccountingSchemaVersion,
			producerUUID,
			now,
		); err != nil {
			return fmt.Errorf("seed compact outbox meta: %w", err)
		}
		frozenInt = 0
	} else if err != nil {
		return fmt.Errorf("read compact outbox meta: %w", err)
	}

	var count int
	var totalBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0) FROM outbox_events`).Scan(&count, &totalBytes); err != nil {
		return fmt.Errorf("scan compact outbox totals: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit compact outbox init: %w", err)
	}

	o.producerUUID = producerUUID
	o.frozen = frozenInt != 0
	o.unackedCount = count
	o.unackedBytes = totalBytes
	return nil
}

func (o *CompactAccountingOutbox) Path() string {
	if o == nil {
		return ""
	}
	return o.path
}

func (o *CompactAccountingOutbox) ProducerUUID() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.producerUUID
}

// SealOrLoadNextBatch returns the active sealed batch if one exists (ensuring
// crash/restart or lost-ACK retries replay the exact same [seq_start, seq_end]
// and digest), or seals the next contiguous prefix of unacknowledged events up
// to MaxBatchCount / MaxBatchBytes. Returns (nil, nil, nil) when empty.
func (o *CompactAccountingOutbox) SealOrLoadNextBatch(ctx context.Context) (*CompactUsageBatchPayload, []byte, error) {
	if o == nil {
		return nil, nil, ErrCompactOutboxUnavailable
	}
	if err := o.Wait(ctx); err != nil {
		return nil, nil, err
	}
	o.dbMu.Lock()
	defer o.dbMu.Unlock()

	var existingRaw []byte
	err := o.db.QueryRowContext(ctx, `SELECT payload_json FROM outbox_sealed_batch WHERE id = 1`).Scan(&existingRaw)
	if err == nil {
		var batch CompactUsageBatchPayload
		if unmarshalErr := json.Unmarshal(existingRaw, &batch); unmarshalErr != nil {
			return nil, nil, fmt.Errorf("unmarshal existing sealed batch: %w", unmarshalErr)
		}
		return &batch, existingRaw, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("query sealed batch: %w", err)
	}

	maxCount := o.cfg.MaxBatchCount
	if maxCount <= 0 {
		maxCount = 256
	}
	maxBytes := o.cfg.MaxBatchBytes
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}

	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin seal batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(
		ctx,
		`SELECT seq, payload_json, payload_bytes FROM outbox_events ORDER BY seq ASC LIMIT ?`,
		maxCount,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("select unacked events for batch: %w", err)
	}

	events := make([]CompactUsageEvent, 0, maxCount)
	var accumulatedBytes int
	for rows.Next() {
		var seq int64
		var payloadJSON []byte
		var payloadBytes int
		if scanErr := rows.Scan(&seq, &payloadJSON, &payloadBytes); scanErr != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan unacked event: %w", scanErr)
		}
		if len(events) > 0 && accumulatedBytes+payloadBytes > maxBytes {
			break
		}
		var ev CompactUsageEvent
		if unmarshalErr := json.Unmarshal(payloadJSON, &ev); unmarshalErr != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("unmarshal event seq %d: %w", seq, unmarshalErr)
		}
		ev.Seq = seq
		if len(events) > 0 && ev.Seq != events[len(events)-1].Seq+1 {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("non-contiguous outbox sequence %d after %d", ev.Seq, events[len(events)-1].Seq)
		}
		events = append(events, ev)
		accumulatedBytes += payloadBytes
	}
	if closeErr := rows.Close(); closeErr != nil {
		return nil, nil, fmt.Errorf("close event rows: %w", closeErr)
	}
	if len(events) == 0 {
		return nil, nil, nil
	}

	seqStart := events[0].Seq
	seqEnd := events[len(events)-1].Seq
	digest, err := ComputeCompactBatchDigest(CompactAccountingSchemaVersion, o.producerUUID, seqStart, seqEnd, events)
	if err != nil {
		return nil, nil, err
	}

	batch := &CompactUsageBatchPayload{
		SchemaVersion: CompactAccountingSchemaVersion,
		ProducerUUID:  o.producerUUID,
		SeqStart:      seqStart,
		SeqEnd:        seqEnd,
		PayloadDigest: digest,
		Events:        events,
	}
	encoded, err := json.Marshal(batch)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal sealed batch: %w", err)
	}

	now := time.Now().UTC().UnixNano()
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO outbox_sealed_batch (id, seq_start, seq_end, event_count, payload_json, payload_digest, created_at) VALUES (1, ?, ?, ?, ?, ?, ?)`,
		seqStart,
		seqEnd,
		len(events),
		encoded,
		digest,
		now,
	); err != nil {
		return nil, nil, fmt.Errorf("persist sealed batch: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit sealed batch: %w", err)
	}
	return batch, encoded, nil
}

// AckBatch deletes acknowledged events through seqEnd, clears the matching
// sealed batch, and advances committed_through_seq in one atomic SQLite commit.
func (o *CompactAccountingOutbox) AckBatch(ctx context.Context, seqStart, seqEnd int64, digest string) error {
	if o == nil {
		return ErrCompactOutboxUnavailable
	}
	o.dbMu.Lock()
	defer o.dbMu.Unlock()

	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ack batch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sealedStart, sealedEnd int64
	var sealedDigest string
	err = tx.QueryRowContext(ctx, `SELECT seq_start, seq_end, payload_digest FROM outbox_sealed_batch WHERE id = 1`).
		Scan(&sealedStart, &sealedEnd, &sealedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load sealed batch for ack: %w", err)
	}
	if sealedStart != seqStart || sealedEnd != seqEnd || sealedDigest != digest {
		return fmt.Errorf("ack mismatch against sealed batch [%d,%d] (%s)", sealedStart, sealedEnd, sealedDigest)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_events WHERE seq >= ? AND seq <= ?`, seqStart, seqEnd); err != nil {
		return fmt.Errorf("delete acked outbox events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM outbox_sealed_batch WHERE id = 1`); err != nil {
		return fmt.Errorf("delete sealed batch row: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE outbox_meta SET committed_through_seq = CASE WHEN committed_through_seq < ? THEN ? ELSE committed_through_seq END, updated_at = ? WHERE id = 1`,
		seqEnd,
		seqEnd,
		now,
	); err != nil {
		return fmt.Errorf("advance committed_through_seq: %w", err)
	}

	var remainingCount int
	var remainingBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0) FROM outbox_events`).
		Scan(&remainingCount, &remainingBytes); err != nil {
		return fmt.Errorf("recount remaining outbox events: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit outbox ack: %w", err)
	}

	o.mu.Lock()
	o.unackedCount = remainingCount
	o.unackedBytes = remainingBytes
	o.mu.Unlock()
	return nil
}

// Status returns the current durable state of the outbox.
func (o *CompactAccountingOutbox) Status(ctx context.Context) (CompactOutboxStatus, error) {
	if o == nil {
		return CompactOutboxStatus{}, ErrCompactOutboxUnavailable
	}
	o.dbMu.Lock()
	defer o.dbMu.Unlock()
	o.mu.Lock()
	status := CompactOutboxStatus{Path: o.path, ProducerUUID: o.producerUUID, Frozen: o.frozen,
		UnackedCount: o.unackedCount, UnackedBytes: o.unackedBytes, ReservedCount: o.reservedCount,
		QueuedCount: o.pendingCount, QueuedBytes: o.pendingBytes, AdmissionFailures: o.admissionFailures,
		DroppedEvents: o.droppedEvents, WriteErrors: o.writeErrors, LastError: o.lastError}
	closed := o.closed
	o.mu.Unlock()
	if closed {
		return status, ErrCompactOutboxUnavailable
	}
	durable, err := readCompactOutboxStatusLocked(ctx, o.db, o.path, status.ReservedCount)
	status.NextSeq, status.CommittedThroughSeq = durable.NextSeq, durable.CommittedThroughSeq
	status.HasSealedBatch, status.SealedSeqStart, status.SealedSeqEnd = durable.HasSealedBatch, durable.SealedSeqStart, durable.SealedSeqEnd
	status.UpdatedAt = durable.UpdatedAt
	return status, err
}

// UnackedCount returns the fast in-memory unacknowledged record count.
func (o *CompactAccountingOutbox) UnackedCount() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.unackedCount + o.pendingCount
}

func readCompactOutboxStatusLocked(ctx context.Context, db *sql.DB, path string, reserved int) (CompactOutboxStatus, error) {
	var status CompactOutboxStatus
	status.Path = path
	status.ReservedCount = reserved

	var frozenInt int
	var updatedNano int64
	if err := db.QueryRowContext(
		ctx,
		`SELECT producer_uuid, next_seq, committed_through_seq, frozen, updated_at FROM outbox_meta WHERE id = 1`,
	).Scan(&status.ProducerUUID, &status.NextSeq, &status.CommittedThroughSeq, &frozenInt, &updatedNano); err != nil {
		return CompactOutboxStatus{}, fmt.Errorf("query outbox_meta status: %w", err)
	}
	status.Frozen = frozenInt != 0
	if updatedNano > 0 {
		status.UpdatedAt = time.Unix(0, updatedNano).UTC()
	}

	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*), COALESCE(SUM(payload_bytes), 0) FROM outbox_events`,
	).Scan(&status.UnackedCount, &status.UnackedBytes); err != nil {
		return CompactOutboxStatus{}, fmt.Errorf("query outbox_events status: %w", err)
	}

	var seqStart, seqEnd int64
	err := db.QueryRowContext(ctx, `SELECT seq_start, seq_end FROM outbox_sealed_batch WHERE id = 1`).Scan(&seqStart, &seqEnd)
	if err == nil {
		status.HasSealedBatch = true
		status.SealedSeqStart = seqStart
		status.SealedSeqEnd = seqEnd
	} else if !errors.Is(err, sql.ErrNoRows) {
		return CompactOutboxStatus{}, fmt.Errorf("query outbox_sealed_batch status: %w", err)
	}
	return status, nil
}

// FreezeAndExport marks this live outbox frozen (rejecting further Reserve/Append),
// checkpoints WAL, and produces a consistent single-file VACUUM INTO snapshot
// plus SHA-256 manifest in exportDir.
func (o *CompactAccountingOutbox) FreezeAndExport(ctx context.Context, exportDir string) (*CompactOutboxExportManifest, error) {
	if o == nil {
		return nil, ErrCompactOutboxUnavailable
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil, ErrCompactOutboxUnavailable
	}
	o.frozen = true
	o.mu.Unlock()
	// Stop new enqueues, then commit the already accepted queue before export.
	if err := o.Wait(ctx); err != nil {
		return nil, err
	}
	o.dbMu.Lock()
	defer o.dbMu.Unlock()
	return freezeAndVacuumExport(ctx, o.db, o.path, exportDir)
}

// InspectCompactOutbox reads status from an outbox SQLite file on disk.
func InspectCompactOutbox(ctx context.Context, outboxPath string) (CompactOutboxStatus, error) {
	cleanPath := filepath.Clean(outboxPath)
	if _, err := os.Stat(cleanPath); err != nil {
		return CompactOutboxStatus{}, fmt.Errorf("stat compact outbox %s: %w", cleanPath, err)
	}
	sqlDB, err := openCompactSQLite(cleanPath)
	if err != nil {
		return CompactOutboxStatus{}, err
	}
	defer func() { _ = sqlDB.Close() }()
	return readCompactOutboxStatusLocked(ctx, sqlDB, cleanPath, 0)
}

// ExportFrozenCompactOutbox freezes an offline/paused outbox SQLite file on disk,
// runs WAL checkpoint + VACUUM INTO into exportDir, and writes manifest.json.
func ExportFrozenCompactOutbox(ctx context.Context, outboxPath, exportDir string) (*CompactOutboxExportManifest, error) {
	cleanPath := filepath.Clean(outboxPath)
	if _, err := os.Stat(cleanPath); err != nil {
		return nil, fmt.Errorf("stat compact outbox %s: %w", cleanPath, err)
	}
	sqlDB, err := openCompactSQLite(cleanPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlDB.Close() }()
	return freezeAndVacuumExport(ctx, sqlDB, cleanPath, exportDir)
}

func freezeAndVacuumExport(ctx context.Context, db *sql.DB, sourcePath, exportDir string) (*CompactOutboxExportManifest, error) {
	if strings.TrimSpace(exportDir) == "" {
		return nil, errors.New("export directory is required")
	}
	cleanDir := filepath.Clean(exportDir)
	if err := os.MkdirAll(cleanDir, 0o750); err != nil {
		return nil, fmt.Errorf("create export directory %s: %w", cleanDir, err)
	}

	snapshotPath := filepath.Join(cleanDir, compactExportSnapshotFilename)
	manifestPath := filepath.Join(cleanDir, compactExportManifestFilename)
	for _, candidate := range []string{snapshotPath, manifestPath} {
		if _, err := os.Stat(candidate); err == nil {
			return nil, fmt.Errorf("export artifact %s already exists; choose a clean export directory", candidate)
		}
	}

	now := time.Now().UTC().UnixNano()
	if _, err := db.ExecContext(ctx, `UPDATE outbox_meta SET frozen = 1, updated_at = ? WHERE id = 1`, now); err != nil {
		return nil, fmt.Errorf("freeze outbox_meta before export: %w", err)
	}
	_, _ = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)

	escapedTarget := strings.ReplaceAll(filepath.ToSlash(snapshotPath), "'", "''")
	if _, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escapedTarget)); err != nil {
		return nil, fmt.Errorf("vacuum outbox snapshot into %s: %w", snapshotPath, err)
	}

	status, err := readCompactOutboxStatusLocked(ctx, db, sourcePath, 0)
	if err != nil {
		return nil, err
	}

	digest, size, err := hashFileSHA256(snapshotPath)
	if err != nil {
		return nil, err
	}

	manifest := &CompactOutboxExportManifest{
		SchemaVersion:       CompactAccountingSchemaVersion,
		SnapshotFile:        compactExportSnapshotFilename,
		SHA256:              digest,
		ByteSize:            size,
		ProducerUUID:        status.ProducerUUID,
		NextSeq:             status.NextSeq,
		CommittedThroughSeq: status.CommittedThroughSeq,
		UnackedCount:        status.UnackedCount,
		UnackedBytes:        status.UnackedBytes,
		HasSealedBatch:      status.HasSealedBatch,
		SealedSeqStart:      status.SealedSeqStart,
		SealedSeqEnd:        status.SealedSeqEnd,
		ExportedAt:          time.Now().UTC(),
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal export manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(manifestBytes, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("write export manifest %s: %w", manifestPath, err)
	}
	return manifest, nil
}

// ImportFrozenCompactOutbox verifies exportDir's manifest & SHA-256 digest and
// installs the snapshot at targetOutboxPath with frozen=0, preserving original
// producer_uuid, event_uuid, contiguous seq numbers, and any sealed batch.
// Fails safely if targetOutboxPath already contains a different producer or
// unacknowledged events.
func ImportFrozenCompactOutbox(ctx context.Context, exportDir, targetOutboxPath string) (*CompactOutboxStatus, error) {
	cleanDir := filepath.Clean(exportDir)
	manifestPath := filepath.Join(cleanDir, compactExportManifestFilename)
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read export manifest %s: %w", manifestPath, err)
	}

	var manifest CompactOutboxExportManifest
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return nil, fmt.Errorf("parse export manifest: %w", err)
	}
	if manifest.SnapshotFile == "" {
		return nil, errors.New("export manifest missing snapshot_file")
	}
	snapshotPath := filepath.Join(cleanDir, filepath.Base(manifest.SnapshotFile))
	actualHash, actualSize, err := hashFileSHA256(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("verify snapshot file: %w", err)
	}
	if !strings.EqualFold(actualHash, manifest.SHA256) || actualSize != manifest.ByteSize {
		return nil, fmt.Errorf("snapshot integrity mismatch: got sha256=%s size=%d, want sha256=%s size=%d", actualHash, actualSize, manifest.SHA256, manifest.ByteSize)
	}

	cleanTarget := filepath.Clean(targetOutboxPath)
	if _, statErr := os.Stat(cleanTarget); statErr == nil {
		existingStatus, inspectErr := InspectCompactOutbox(ctx, cleanTarget)
		if inspectErr != nil {
			return nil, fmt.Errorf("target outbox %s exists and could not be inspected: %w", cleanTarget, inspectErr)
		}
		if existingStatus.UnackedCount > 0 {
			return nil, fmt.Errorf("target outbox %s already has %d unacked events; refuse destructive overwrite", cleanTarget, existingStatus.UnackedCount)
		}
		if existingStatus.ProducerUUID != manifest.ProducerUUID && existingStatus.NextSeq > 1 {
			return nil, fmt.Errorf("target outbox %s belongs to another producer (%s) with history; use a clean target path", cleanTarget, existingStatus.ProducerUUID)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if rmErr := os.Remove(cleanTarget + suffix); rmErr != nil && !os.IsNotExist(rmErr) {
				return nil, fmt.Errorf("remove empty target outbox %s: %w", cleanTarget+suffix, rmErr)
			}
		}
	}

	if dir := filepath.Dir(cleanTarget); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create target outbox dir %s: %w", dir, err)
		}
	}

	tmpTarget := cleanTarget + ".import.tmp"
	_ = os.Remove(tmpTarget)
	if err := copyFileExact(snapshotPath, tmpTarget); err != nil {
		_ = os.Remove(tmpTarget)
		return nil, err
	}

	importedDB, err := openCompactSQLite(tmpTarget)
	if err != nil {
		_ = os.Remove(tmpTarget)
		return nil, fmt.Errorf("open imported snapshot: %w", err)
	}
	now := time.Now().UTC().UnixNano()
	if _, err := importedDB.ExecContext(ctx, `UPDATE outbox_meta SET frozen = 0, updated_at = ? WHERE id = 1`, now); err != nil {
		_ = importedDB.Close()
		_ = os.Remove(tmpTarget)
		return nil, fmt.Errorf("unfreeze imported outbox: %w", err)
	}
	_, _ = importedDB.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	status, statusErr := readCompactOutboxStatusLocked(ctx, importedDB, cleanTarget, 0)
	closeErr := importedDB.Close()
	if statusErr != nil || closeErr != nil {
		_ = os.Remove(tmpTarget)
		return nil, errors.Join(statusErr, closeErr)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(tmpTarget + suffix)
	}

	if err := os.Rename(tmpTarget, cleanTarget); err != nil {
		_ = os.Remove(tmpTarget)
		return nil, fmt.Errorf("install imported outbox at %s: %w", cleanTarget, err)
	}
	return &status, nil
}

func hashFileSHA256(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFileExact(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create destination %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("fsync %s: %w", dst, err)
	}
	return out.Close()
}
