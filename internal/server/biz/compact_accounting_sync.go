package biz

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/compactusagebatch"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/cluster"
)

const (
	ClusterAccountingEndpointPath = "/__axon/cluster/accounting/batches"

	HeaderClusterNodeID    = "X-Axon-Cluster-Node-Id"
	HeaderClusterTimestamp = "X-Axon-Cluster-Timestamp"
	HeaderClusterSignature = "X-Axon-Cluster-Signature"

	clusterAccountingKeyDomain = "axonhub-cluster-accounting-v1:"
	maxClusterSignatureSkew    = 5 * time.Minute
)

var ErrClusterAccountingAuth = errors.New("cluster accounting authentication failed")

// CompactUsageBatchAck is returned by the control node once a batch is
// durably committed (or recognized as an exact idempotent replay).
type CompactUsageBatchAck struct {
	ProducerUUID  string `json:"producer_uuid"`
	SeqStart      int64  `json:"seq_start"`
	SeqEnd        int64  `json:"seq_end"`
	EventCount    int    `json:"event_count"`
	PayloadDigest string `json:"payload_digest"`
	Idempotent    bool   `json:"idempotent"`
}

// CompactAccountingServiceParams wires dependencies for the control-node
// batch receiver and the data-node local SQLite outbox syncer.
type CompactAccountingServiceParams struct {
	fx.In

	Ent           *ent.Client
	SystemService *SystemService
	UsageLog      *UsageLogService
	Node          cluster.Config `optional:"true"`
}

// CompactAccountingService owns both:
//  1. Control-node idempotent batch ingestion into usage_logs + compact_usage_batches;
//  2. Non-control data-node local SQLite WAL outbox + periodic/drain syncer.
type CompactAccountingService struct {
	*AbstractService

	primary  *ent.Client
	system   *SystemService
	usageLog *UsageLogService
	node     cluster.Config
	cfg      cluster.CompactAccountingConfig

	outbox     *CompactAccountingOutbox
	httpClient *http.Client

	flushMu   sync.Mutex
	mu        sync.Mutex
	started   bool
	cancel    context.CancelFunc
	done      chan struct{}
	lastErrMu sync.RWMutex
	lastErr   string

	// TransportHookForTest allows deterministic injection of lost-ACK or
	// transient network errors around the real HTTP call in tests.
	TransportHookForTest func(ctx context.Context, req *http.Request, do func(*http.Request) (*http.Response, error)) (*http.Response, error)

	syncedBatches atomic.Int64
	syncedEvents  atomic.Int64
}

func NewCompactAccountingService(params CompactAccountingServiceParams) *CompactAccountingService {
	var primary *ent.Client
	if params.Ent != nil {
		primary = observationPrimaryClient(params.Ent)
	}
	cfg := params.Node.CompactAccounting.WithDefaults(params.Node.NodeID)
	svc := &CompactAccountingService{
		AbstractService: &AbstractService{db: primary},
		primary:         primary,
		system:          params.SystemService,
		usageLog:        params.UsageLog,
		node:            params.Node,
		cfg:             cfg,
		httpClient: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
	}
	return svc
}

// Active reports whether compact local SQLite accounting is enabled on this node.
func (s *CompactAccountingService) Active() bool {
	return s != nil && s.node.CompactAccountingActive()
}

// Outbox returns the active local SQLite outbox (non-nil after Start on an active data node).
func (s *CompactAccountingService) Outbox() *CompactAccountingOutbox {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outbox
}

// Start opens the local SQLite outbox (when CompactAccountingActive) and launches
// startup replay + low-frequency background synchronization.
func (s *CompactAccountingService) Start(ctx context.Context) error {
	if s == nil || !s.Active() {
		return nil
	}

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil
	}
	outbox, err := OpenCompactAccountingOutbox(s.cfg, s.node.NodeID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	bgCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.outbox = outbox
	s.cancel = cancel
	s.done = done
	s.started = true
	s.mu.Unlock()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error(context.Background(), "compact accounting sync loop panicked", log.Any("panic", r))
			}
			close(done)
		}()
		s.syncLoop(bgCtx)
	}()
	return nil
}

// Stop halts background periodic uploads and closes the SQLite outbox handle.
// Unacknowledged events remain safely persisted in the WAL file for next startup
// or frozen export.
func (s *CompactAccountingService) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	cancel := s.cancel
	done := s.done
	outbox := s.outbox
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// Close admission even if this shutdown deadline expires. Retain the handle
	// until both workers finish so another Stop can await the same drain.
	if outbox != nil {
		if err := outbox.Close(ctx); err != nil {
			return err
		}
	}
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	s.started = false
	s.cancel, s.done, s.outbox = nil, nil, nil
	s.mu.Unlock()
	return nil
}

// UnackedCount returns the current number of locally persisted compact usage
// events that have not yet received a central ACK.
func (s *CompactAccountingService) UnackedCount(ctx context.Context) (int, error) {
	outbox := s.Outbox()
	if outbox == nil {
		return 0, nil
	}
	st, err := outbox.Status(ctx)
	if err != nil {
		return outbox.UnackedCount(), err
	}
	return st.UnackedCount + st.QueuedCount, nil
}

// LastSyncError returns the most recent upload error string if any.
func (s *CompactAccountingService) LastSyncError() string {
	if s == nil {
		return ""
	}
	s.lastErrMu.RLock()
	defer s.lastErrMu.RUnlock()
	return s.lastErr
}

func (s *CompactAccountingService) setLastSyncError(err error) {
	s.lastErrMu.Lock()
	defer s.lastErrMu.Unlock()
	if err == nil {
		s.lastErr = ""
	} else {
		s.lastErr = err.Error()
	}
}

func (s *CompactAccountingService) syncLoop(ctx context.Context) {
	// Startup replay: immediately attempt to flush any unacknowledged batches
	// left behind by an earlier crash or restart without blocking HTTP startup.
	if err := s.Flush(ctx); err != nil && ctx.Err() == nil {
		log.Warn(ctx, "initial compact accounting replay did not complete; will retry on schedule", log.Cause(err))
	}

	interval := s.cfg.SyncInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Flush(ctx); err != nil && ctx.Err() == nil {
				log.Warn(ctx, "scheduled compact accounting batch upload failed", log.Cause(err))
			}
		}
	}
}

// Flush immediately seals and uploads all locally pending compact usage batches
// until UnackedCount reaches 0 or an error/cancellation occurs.
func (s *CompactAccountingService) Flush(ctx context.Context) error {
	if s == nil || !s.Active() {
		return nil
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	outbox := s.Outbox()
	if outbox == nil {
		return ErrCompactOutboxUnavailable
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, rawBytes, err := outbox.SealOrLoadNextBatch(ctx)
		if err != nil {
			s.setLastSyncError(err)
			return err
		}
		if batch == nil {
			s.setLastSyncError(nil)
			return nil
		}

		ack, err := s.sendSealedBatch(ctx, batch, rawBytes)
		if err != nil {
			s.setLastSyncError(err)
			return err
		}
		if err := outbox.AckBatch(ctx, ack.SeqStart, ack.SeqEnd, ack.PayloadDigest); err != nil {
			s.setLastSyncError(err)
			return err
		}
		s.syncedBatches.Add(1)
		s.syncedEvents.Add(int64(len(batch.Events)))
	}
}

func (s *CompactAccountingService) sendSealedBatch(ctx context.Context, batch *CompactUsageBatchPayload, rawBytes []byte) (*CompactUsageBatchAck, error) {
	controlOrigin := strings.TrimRight(s.node.Nodes[s.node.ControlNode], "/")
	if controlOrigin == "" {
		return nil, errors.New("control node origin is not configured")
	}
	signingKey, err := s.deriveSigningKey(ctx)
	if err != nil {
		return nil, err
	}

	timeout := s.cfg.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	endpoint := controlOrigin + ClusterAccountingEndpointPath
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(rawBytes))
	if err != nil {
		return nil, fmt.Errorf("build compact batch request: %w", err)
	}
	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	sig := computeClusterAccountingSignature(signingKey, s.node.NodeID, ts, rawBytes)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderClusterNodeID, s.node.NodeID)
	req.Header.Set(HeaderClusterTimestamp, ts)
	req.Header.Set(HeaderClusterSignature, sig)

	doCall := s.httpClient.Do
	var resp *http.Response
	if hook := s.TransportHookForTest; hook != nil {
		resp, err = hook(reqCtx, req, doCall)
	} else {
		resp, err = doCall(req)
	}
	if err != nil {
		return nil, fmt.Errorf("post compact batch [%d,%d]: %w", batch.SeqStart, batch.SeqEnd, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read compact batch ack: %w", err)
	}
	if resp.StatusCode == http.StatusConflict {
		return nil, fmt.Errorf("%w: status=409 body=%s", ErrCompactBatchConflict, strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("control node rejected compact batch [%d,%d]: status=%d body=%s", batch.SeqStart, batch.SeqEnd, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var ack CompactUsageBatchAck
	if err := json.Unmarshal(respBody, &ack); err != nil {
		return nil, fmt.Errorf("decode compact batch ack: %w", err)
	}
	if ack.ProducerUUID != batch.ProducerUUID || ack.SeqStart != batch.SeqStart || ack.SeqEnd != batch.SeqEnd || ack.PayloadDigest != batch.PayloadDigest {
		return nil, fmt.Errorf("compact batch ack mismatch: got (%s [%d,%d] %s), want (%s [%d,%d] %s)",
			ack.ProducerUUID, ack.SeqStart, ack.SeqEnd, ack.PayloadDigest,
			batch.ProducerUUID, batch.SeqStart, batch.SeqEnd, batch.PayloadDigest)
	}
	return &ack, nil
}

// VerifyClusterRequest authenticates an inbound node-to-control batch upload.
func (s *CompactAccountingService) VerifyClusterRequest(ctx context.Context, nodeID, timestampHeader, signatureHeader string, rawBody []byte) error {
	if s == nil || !s.node.Enabled || !s.node.IsControl() {
		return fmt.Errorf("%w: control node cluster receiver not enabled", ErrClusterAccountingAuth)
	}
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" || nodeID == s.node.ControlNode {
		return fmt.Errorf("%w: invalid sender node_id %q", ErrClusterAccountingAuth, nodeID)
	}
	if _, known := s.node.Nodes[nodeID]; !known {
		return fmt.Errorf("%w: unknown sender node_id %q", ErrClusterAccountingAuth, nodeID)
	}

	unixSec, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: invalid timestamp", ErrClusterAccountingAuth)
	}
	skew := time.Since(time.Unix(unixSec, 0))
	if skew < -maxClusterSignatureSkew || skew > maxClusterSignatureSkew {
		return fmt.Errorf("%w: timestamp skew %s exceeds window", ErrClusterAccountingAuth, skew)
	}

	signingKey, err := s.deriveSigningKey(ctx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrClusterAccountingAuth, err)
	}
	expected := computeClusterAccountingSignature(signingKey, nodeID, strings.TrimSpace(timestampHeader), rawBody)
	gotBytes, err := hex.DecodeString(strings.TrimSpace(signatureHeader))
	if err != nil {
		return fmt.Errorf("%w: malformed signature hex", ErrClusterAccountingAuth)
	}
	expBytes, _ := hex.DecodeString(expected)
	if !hmac.Equal(gotBytes, expBytes) {
		return fmt.Errorf("%w: signature mismatch", ErrClusterAccountingAuth)
	}
	return nil
}

func (s *CompactAccountingService) deriveSigningKey(ctx context.Context) ([]byte, error) {
	secret := strings.TrimSpace(s.cfg.SharedSecret)
	if secret == "" {
		if s.system == nil {
			return nil, errors.New("system service unavailable for cluster accounting key derivation")
		}
		bypassCtx := authz.WithSystemBypass(ctx, "cluster-accounting-signing-key")
		sysSecret, err := s.system.SecretKey(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("read system secret key: %w", err)
		}
		secret = strings.TrimSpace(sysSecret)
	}
	if secret == "" {
		return nil, errors.New("empty cluster accounting secret")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(clusterAccountingKeyDomain))
	return mac.Sum(nil), nil
}

func computeClusterAccountingSignature(signingKey []byte, nodeID, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, signingKey)
	_, _ = mac.Write([]byte(nodeID))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// IngestBatch validates and commits a compact usage batch into the central
// usage_logs ledger + compact_usage_batches receipt table in a single database
// transaction, guaranteeing idempotent replay and zero double-counting.
func (s *CompactAccountingService) IngestBatch(ctx context.Context, nodeID string, batch CompactUsageBatchPayload) (*CompactUsageBatchAck, error) {
	if s == nil || s.primary == nil {
		return nil, errors.New("compact accounting receiver not initialized")
	}
	if err := validateCompactBatch(batch); err != nil {
		return nil, err
	}

	expectedDigest, err := ComputeCompactBatchDigest(batch.SchemaVersion, batch.ProducerUUID, batch.SeqStart, batch.SeqEnd, batch.Events)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(batch.PayloadDigest, expectedDigest) {
		return nil, fmt.Errorf("%w: payload_digest mismatch (got %s, computed %s)", ErrCompactBatchConflict, batch.PayloadDigest, expectedDigest)
	}

	txCtx := authz.WithSystemBypass(ent.NewContext(ctx, s.primary), "compact-accounting-ingest")
	var ack *CompactUsageBatchAck
	var insertedNew bool

	err = s.RunInTransaction(txCtx, func(innerCtx context.Context) error {
		client := ent.FromContext(innerCtx)
		if lockErr := acquireProducerAdvisoryLock(innerCtx, client, batch.ProducerUUID); lockErr != nil {
			return lockErr
		}

		// 1. Check exact (producer_uuid, seq_start, seq_end) receipt first.
		existing, lookupErr := client.CompactUsageBatch.Query().
			Where(
				compactusagebatch.ProducerUUIDEQ(batch.ProducerUUID),
				compactusagebatch.SeqStartEQ(batch.SeqStart),
				compactusagebatch.SeqEndEQ(batch.SeqEnd),
			).
			Only(innerCtx)
		if lookupErr == nil {
			if !strings.EqualFold(existing.PayloadDigest, expectedDigest) || existing.EventCount != len(batch.Events) {
				return fmt.Errorf("%w: existing receipt [%d,%d] has digest %s (count=%d), incoming has %s (count=%d)",
					ErrCompactBatchConflict, batch.SeqStart, batch.SeqEnd, existing.PayloadDigest, existing.EventCount, expectedDigest, len(batch.Events))
			}
			ack = &CompactUsageBatchAck{
				ProducerUUID:  existing.ProducerUUID,
				SeqStart:      existing.SeqStart,
				SeqEnd:        existing.SeqEnd,
				EventCount:    existing.EventCount,
				PayloadDigest: existing.PayloadDigest,
				Idempotent:    true,
			}
			return nil
		}
		if !ent.IsNotFound(lookupErr) {
			return fmt.Errorf("query existing compact batch receipt: %w", lookupErr)
		}

		// 2. Reject any overlapping sequence range for the same producer_uuid.
		overlapping, overlapErr := client.CompactUsageBatch.Query().
			Where(
				compactusagebatch.ProducerUUIDEQ(batch.ProducerUUID),
				compactusagebatch.SeqStartLTE(batch.SeqEnd),
				compactusagebatch.SeqEndGTE(batch.SeqStart),
			).
			Exist(innerCtx)
		if overlapErr != nil {
			return fmt.Errorf("check overlapping compact batch range: %w", overlapErr)
		}
		if overlapping {
			return fmt.Errorf("%w: overlapping sequence range [%d,%d] for producer %s",
				ErrCompactBatchConflict, batch.SeqStart, batch.SeqEnd, batch.ProducerUUID)
		}

		// 3. Insert each usage event idempotently by event_id into usage_logs.
		eventIDs := make([]string, len(batch.Events))
		for i := range batch.Events {
			eventIDs[i] = batch.Events[i].EventUUID
		}
		alreadyPresent := make(map[string]struct{})
		if len(eventIDs) > 0 {
			existingLogs, queryErr := client.UsageLog.Query().
				Where(usagelog.EventIDIn(eventIDs...)).
				Select(usagelog.FieldEventID).
				All(innerCtx)
			if queryErr != nil {
				return fmt.Errorf("query existing compact usage_logs by event_id: %w", queryErr)
			}
			for _, row := range existingLogs {
				if row.EventID != nil {
					alreadyPresent[*row.EventID] = struct{}{}
				}
			}
		}

		builders := make([]*ent.UsageLogCreate, 0, len(batch.Events))
		for _, ev := range batch.Events {
			if _, ok := alreadyPresent[ev.EventUUID]; ok {
				continue
			}
			capturedAt := ev.CapturedAt.UTC()
			source := usagelog.Source(ev.Source)
			if source == "" {
				source = usagelog.SourceAPI
			}
			mut := client.UsageLog.Create().
				SetEventID(ev.EventUUID).
				SetProjectID(ev.ProjectID).
				SetChannelID(ev.ChannelID).
				SetModelID(ev.ModelID).
				SetSource(source).
				SetFormat(ev.Format).
				SetPromptTokens(ev.Usage.PromptTokens).
				SetCompletionTokens(ev.Usage.CompletionTokens).
				SetTotalTokens(ev.Usage.TotalTokens).
				SetNillableTotalCost(ev.TotalCost).
				SetCostItems(ev.CostItems).
				SetCreatedAt(capturedAt).
				SetUpdatedAt(capturedAt)
			if ev.APIKeyID > 0 {
				mut.SetAPIKeyID(ev.APIKeyID)
			}
			if ev.PriceReferenceID != "" {
				mut.SetCostPriceReferenceID(ev.PriceReferenceID)
			}
			if ev.Usage.PromptTokensDetails != nil {
				mut.
					SetPromptAudioTokens(ev.Usage.PromptTokensDetails.AudioTokens).
					SetPromptCachedTokens(ev.Usage.PromptTokensDetails.CachedTokens).
					SetPromptWriteCachedTokens(ev.Usage.PromptTokensDetails.WriteCachedTokens).
					SetPromptWriteCachedTokens5m(ev.Usage.PromptTokensDetails.WriteCached5MinTokens).
					SetPromptWriteCachedTokens1h(ev.Usage.PromptTokensDetails.WriteCached1HourTokens)
			}
			if ev.Usage.CompletionTokensDetails != nil {
				mut.
					SetCompletionAudioTokens(ev.Usage.CompletionTokensDetails.AudioTokens).
					SetCompletionReasoningTokens(ev.Usage.CompletionTokensDetails.ReasoningTokens).
					SetCompletionAcceptedPredictionTokens(ev.Usage.CompletionTokensDetails.AcceptedPredictionTokens).
					SetCompletionRejectedPredictionTokens(ev.Usage.CompletionTokensDetails.RejectedPredictionTokens)
			}
			builders = append(builders, mut)
		}

		if len(builders) > 0 {
			if err := client.UsageLog.CreateBulk(builders...).
				OnConflict(entsql.ConflictColumns(usagelog.FieldEventID)).
				Ignore().
				Exec(innerCtx); err != nil {
				return fmt.Errorf("bulk insert compact usage_logs: %w", err)
			}
			insertedNew = true
		}

		// 4. Persist the durable batch receipt inside the same transaction.
		receipt, createErr := client.CompactUsageBatch.Create().
			SetProducerUUID(batch.ProducerUUID).
			SetNodeID(nodeID).
			SetSeqStart(batch.SeqStart).
			SetSeqEnd(batch.SeqEnd).
			SetEventCount(len(batch.Events)).
			SetPayloadDigest(expectedDigest).
			SetSchemaVersion(batch.SchemaVersion).
			Save(innerCtx)
		if createErr != nil {
			return fmt.Errorf("save compact usage batch receipt: %w", createErr)
		}

		ack = &CompactUsageBatchAck{
			ProducerUUID:  receipt.ProducerUUID,
			SeqStart:      receipt.SeqStart,
			SeqEnd:        receipt.SeqEnd,
			EventCount:    receipt.EventCount,
			PayloadDigest: receipt.PayloadDigest,
			Idempotent:    false,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if insertedNew && s.usageLog != nil && s.usageLog.OnUsageLogCreated != nil {
		s.usageLog.OnUsageLogCreated()
	}
	return ack, nil
}

func validateCompactBatch(batch CompactUsageBatchPayload) error {
	if batch.SchemaVersion != CompactAccountingSchemaVersion {
		return fmt.Errorf("unsupported compact batch schema_version %d", batch.SchemaVersion)
	}
	if strings.TrimSpace(batch.ProducerUUID) == "" {
		return errors.New("compact batch producer_uuid is required")
	}
	if len(batch.Events) == 0 {
		return errors.New("compact batch events cannot be empty")
	}
	if batch.SeqStart <= 0 || batch.SeqEnd < batch.SeqStart {
		return fmt.Errorf("invalid compact batch sequence range [%d,%d]", batch.SeqStart, batch.SeqEnd)
	}
	expectedCount := int(batch.SeqEnd - batch.SeqStart + 1)
	if len(batch.Events) != expectedCount {
		return fmt.Errorf("%w: batch range [%d,%d] expects %d contiguous events, got %d",
			ErrCompactBatchConflict, batch.SeqStart, batch.SeqEnd, expectedCount, len(batch.Events))
	}
	seenUUIDs := make(map[string]struct{}, len(batch.Events))
	for i, ev := range batch.Events {
		wantSeq := batch.SeqStart + int64(i)
		if ev.Seq != wantSeq {
			return fmt.Errorf("%w: event index %d has seq %d, want %d", ErrCompactBatchConflict, i, ev.Seq, wantSeq)
		}
		if strings.TrimSpace(ev.EventUUID) == "" {
			return fmt.Errorf("event seq %d missing event_uuid", ev.Seq)
		}
		if _, dup := seenUUIDs[ev.EventUUID]; dup {
			return fmt.Errorf("%w: duplicate event_uuid %s in batch", ErrCompactBatchConflict, ev.EventUUID)
		}
		seenUUIDs[ev.EventUUID] = struct{}{}
		if ev.ProjectID <= 0 || strings.TrimSpace(ev.ModelID) == "" || ev.CapturedAt.IsZero() {
			return fmt.Errorf("event seq %d missing required project_id, model_id, or captured_at", ev.Seq)
		}
	}
	return nil
}

func acquireProducerAdvisoryLock(ctx context.Context, client *ent.Client, producerUUID string) error {
	if client == nil || client.Driver() == nil || client.Driver().Dialect() != dialect.Postgres {
		return nil
	}
	sum := sha256.Sum256([]byte("axonhub-compact-producer:" + producerUUID))
	lockKey := int64(binary.BigEndian.Uint64(sum[:8]))
	var rows entsql.Rows
	if err := client.Driver().Query(ctx, "SELECT pg_advisory_xact_lock($1)", []any{lockKey}, &rows); err != nil {
		return fmt.Errorf("acquire producer advisory xact lock: %w", err)
	}
	return rows.Close()
}
