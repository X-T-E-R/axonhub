package biz

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/pkg/chunkbuffer"
)

func NewRequestServiceWithObservationWriter(client *ent.Client, system *SystemService, usage *UsageLogService, storage *DataStorageService, live *LiveStreamRegistry, bodyWriter *ManagedRequestBodyWriter, writer *ForwardingObservationWriter) *RequestService {
	s := NewRequestServiceWithManagedRequestBodyWriter(client, system, usage, storage, live, bodyWriter)
	s.ObservationWriter = writer
	system.observationWriter = writer
	writer.system = system
	writer.payloadLane.system = system
	writer.primary = observationPrimaryClient(client)
	writer.payloadLane.primary = writer.primary
	return s
}

func NewQuotaServiceWithObservationWriter(client *ent.Client, system *SystemService, writer *ForwardingObservationWriter) *QuotaService {
	s := NewQuotaService(observationPrimaryClient(client), system)
	s.observation = writer
	return s
}

// This view routes nonce recovery and accounting snapshots to the write database.
// It does not own or close the shared driver.
func observationPrimaryClient(client *ent.Client) *ent.Client {
	if router, ok := client.Driver().(interface{ PrimaryDriver() dialect.Driver }); ok {
		return ent.NewClient(ent.Driver(router.PrimaryDriver()))
	}
	return client
}

func (s *RequestService) ForwardingObservationContext(ctx context.Context) context.Context {
	if s.ObservationWriter == nil {
		return ctx
	}
	return s.ObservationWriter.WithScope(ctx)
}

func (s *TraceService) ForwardingObservationContext(ctx context.Context) context.Context {
	// Legacy standalone trace readers can be constructed without request services.
	if s.requestService == nil {
		return ctx
	}
	return s.requestService.ForwardingObservationContext(ctx)
}

func (s *ThreadService) ForwardingObservationContext(ctx context.Context) context.Context {
	return s.traceService.ForwardingObservationContext(ctx)
}

// ForwardingObservationWriter serializes optional persistence independently of
// transport lifetimes. Queue accounting includes running work, not just queued
// work, so a blocked database cannot turn the queue into unbounded retention.
type ForwardingObservationWriter struct {
	config         ManagedRequestBodyWriterConfig
	mu             sync.Mutex
	jobs           chan observationJob
	bytes          int64
	items          int
	started        bool
	stopping       bool
	idle           chan struct{}
	done           chan struct{}
	ctx            context.Context
	cancel         context.CancelFunc
	nextID         atomic.Int64
	policy         atomic.Pointer[StoragePolicy]
	system         *SystemService
	primary        *ent.Client
	pendingRecords map[*observationPendingUsage]struct{}
	scopes         map[*observationScope]struct{}
	payloadLane    *ForwardingObservationWriter
	payloadOnly    bool
}

type observationJob struct {
	ctx   context.Context
	bytes int64
	run   func(context.Context) error
	usage *observationPendingUsage
}

type (
	observationContextKey         struct{}
	observationPolicyContextKey   struct{}
	observationPersistenceKey     struct{}
	observationPersistenceContext struct {
		writer   *ForwardingObservationWriter
		finished <-chan struct{}
	}
)

const (
	observationUsageReservationBytes    int64 = 8192
	observationTerminalReservationBytes int64 = 16384
)

var (
	errObservationParentUnavailable = errors.New("observation parent was not admitted")
	errObservationQueueUnavailable  = errors.New("forwarding observation queue unavailable or full")
)

// An observation scope owns its local-to-database ID map. Only the single
// persistence worker accesses ids; forwarding entities are never mutated by it.
type observationScope struct {
	writer            *ForwardingObservationWriter
	ids               map[int]int
	reservedCore      int
	reservedTerminal  int
	terminalCredits   map[int]bool
	executionChannels map[int]*Channel
	reservedUsage     bool
	usageSubmitted    bool
	ended             bool
	liveMu            sync.Mutex
	liveClosed        bool
	liveBindings      map[int]func(int) func()
	liveCleanup       []func()
	boundIDs          sync.Map
}

func NewForwardingObservationWriter(config ManagedRequestBodyWriterConfig) *ForwardingObservationWriter {
	w := newForwardingObservationLane(config)
	w.payloadLane = newForwardingObservationLane(config)
	w.payloadLane.payloadOnly = true
	return w
}

func newForwardingObservationLane(config ManagedRequestBodyWriterConfig) *ForwardingObservationWriter {
	config = config.withDefaults()
	idle := make(chan struct{})
	close(idle)
	return &ForwardingObservationWriter{config: config, jobs: make(chan observationJob, config.MaxItems), idle: idle, done: make(chan struct{}), pendingRecords: make(map[*observationPendingUsage]struct{}), scopes: make(map[*observationScope]struct{})}
}

func (w *ForwardingObservationWriter) Start(ctx context.Context) error {
	if w.primary != nil {
		ctx = ent.NewContext(ctx, w.primary)
	}
	if !w.payloadOnly && w.system != nil {
		policy, err := w.system.StoragePolicyFresh(authz.WithSystemBypass(ctx, "forwarding-observation-startup"))
		if err != nil {
			return err
		}
		w.policy.Store(policy)
	}
	if w.payloadLane != nil {
		if err := w.payloadLane.Start(context.Background()); err != nil {
			return err
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return nil
	}
	if w.stopping {
		return errors.New("observation writer stopped")
	}
	w.started = true
	w.ctx, w.cancel = context.WithCancel(context.Background())
	go func() {
		defer close(w.done)
		defer w.abandonPending()
		defer func() {
			if p := recover(); p != nil {
				log.Error(context.Background(), "Observation worker panicked", log.Any("panic", p))
			}
		}()
		for {
			select {
			case <-w.ctx.Done():
				return
			case job := <-w.jobs:
				w.run(job)
			}
		}
	}()
	return nil
}

func (w *ForwardingObservationWriter) abandonPending() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopping = true
	for scope := range w.scopes {
		reserved := scope.reservedCore
		reservedBytes := int64(reserved) * 4096
		if scope.reservedUsage {
			reserved++
			reservedBytes += observationUsageReservationBytes
		}
		terminals := scope.reservedTerminal + len(scope.terminalCredits)
		reserved += terminals
		reservedBytes += int64(terminals) * observationTerminalReservationBytes
		w.items -= reserved
		w.bytes -= reservedBytes
		scope.reservedCore = 0
		scope.reservedUsage = false
		scope.reservedTerminal = 0
		clear(scope.terminalCredits)
		delete(w.scopes, scope)
	}
	for {
		select {
		case job := <-w.jobs:
			w.items--
			delete(w.pendingRecords, job.usage)
			w.bytes -= job.bytes
			metrics.RecordManagedObservabilityFailure(context.Background(), "forwarding_observation", "shutdown_abandoned")
		default:
			select {
			case <-w.idle:
			default:
				if w.items == 0 {
					close(w.idle)
				}
			}
			return
		}
	}
}

func (w *ForwardingObservationWriter) run(job observationJob) {
	defer func() {
		w.mu.Lock()
		w.items--
		delete(w.pendingRecords, job.usage)
		w.bytes -= job.bytes
		if w.items == 0 {
			close(w.idle)
		}
		w.mu.Unlock()
	}()
	for attempt := 1; w.ctx.Err() == nil; attempt++ {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(job.ctx), w.config.AttemptTimeout)
		stop := context.AfterFunc(w.ctx, cancel)
		ctx = authz.WithSystemBypass(ctx, "forwarding-observation")
		ctx = context.WithValue(ctx, observationPolicyContextKey{}, (*StoragePolicy)(nil))
		if w.system != nil {
			if policy, err := w.system.StoragePolicyFresh(ctx); err == nil {
				w.policy.Store(policy)
				ctx = context.WithValue(ctx, observationPolicyContextKey{}, policy)
			}
		}
		finished := make(chan struct{})
		if !w.payloadOnly {
			ctx = context.WithValue(ctx, observationPersistenceKey{}, observationPersistenceContext{writer: w, finished: finished})
		}
		err := job.run(ctx)
		close(finished)
		stop()
		cancel()
		if err == nil {
			return
		}
		metrics.RecordManagedObservabilityFailure(context.Background(), "forwarding_observation", "write_failed")
		log.Warn(ctx, "Forwarding observation persistence failed", log.Cause(err))
		if errors.Is(err, errObservationParentUnavailable) {
			return
		}
		if w.payloadOnly && attempt >= w.config.MaxAttempts && !errors.Is(err, errObservationCompletionPending) {
			return
		}
		// Accepted work is retained until it commits, or process shutdown.
		// Retrying consumes the same bounded slot and cannot launch goroutines.
		timer := time.NewTimer(w.config.AttemptTimeout)
		select {
		case <-timer.C:
		case <-w.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (w *ForwardingObservationWriter) Stop(ctx context.Context) (err error) {
	if w.payloadLane != nil {
		defer func() { err = errors.Join(err, w.payloadLane.Stop(ctx)) }()
		// Payload completion patches return to the core FIFO. Drain both lanes
		// before closing core admission; otherwise healthy payloads lose markers.
		_ = w.Wait(ctx)
	}
	w.mu.Lock()
	w.stopping = true
	started, idle := w.started, w.idle
	w.mu.Unlock()
	if !started {
		return nil
	}
	select {
	case <-idle:
	case <-ctx.Done():
	}
	w.cancel()
	select {
	case <-w.done:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *ForwardingObservationWriter) WithScope(ctx context.Context) context.Context {
	if observationFromContext(ctx) != nil {
		return ctx
	}
	scope := &observationScope{writer: w, ids: make(map[int]int), terminalCredits: make(map[int]bool)}
	w.mu.Lock()
	reservationBytes := 2*4096 + observationUsageReservationBytes + 2*observationTerminalReservationBytes
	if w.started && !w.stopping && w.items+5 <= w.config.MaxItems && w.bytes+reservationBytes <= int64(w.config.MaxBytesMiB)<<20 {
		if w.items == 0 {
			w.idle = make(chan struct{})
		}
		w.items += 5
		w.bytes += reservationBytes
		scope.reservedCore = 2
		scope.reservedTerminal = 2
		scope.reservedUsage = true
		w.scopes[scope] = struct{}{}
	}
	w.mu.Unlock()
	return context.WithValue(ctx, observationContextKey{}, scope)
}

func observationFromContext(ctx context.Context) *observationScope {
	scope, _ := ctx.Value(observationContextKey{}).(*observationScope)
	return scope
}

func (s *observationScope) newID() int { return -int(s.writer.nextID.Add(1)) }

func (s *observationScope) executionChannel(id int, channel *Channel) *Channel {
	if channel != nil {
		return channel
	}
	s.writer.mu.Lock()
	defer s.writer.mu.Unlock()
	return s.executionChannels[id]
}

func (s *observationScope) bindID(id, actual int) {
	s.ids[id] = actual
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	s.boundIDs.Store(id, actual)
	if bind := s.liveBindings[id]; bind != nil && !s.liveClosed {
		s.liveCleanup = append(s.liveCleanup, bind(actual))
		delete(s.liveBindings, id)
	}
}

func BindLiveObservation(ctx context.Context, localID int, registry *LiveStreamRegistry, buffer *chunkbuffer.Buffer, execution bool) {
	s := observationFromContext(ctx)
	if s == nil || localID >= 0 || buffer == nil {
		return
	}
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.liveClosed {
		return
	}
	bind := func(actual int) func() {
		if execution {
			registry.RegisterExecution(actual, buffer)
			return func() { registry.UnregisterExecution(actual) }
		}
		registry.RegisterRequest(actual, buffer)
		return func() { registry.UnregisterRequest(actual) }
	}
	if actual, ok := s.boundIDs.Load(localID); ok {
		actualID := actual.(int) //nolint:forcetypeassert // bindID stores only integer IDs in boundIDs.
		s.liveCleanup = append(s.liveCleanup, bind(actualID))
	} else {
		if s.liveBindings == nil {
			s.liveBindings = make(map[int]func(int) func())
		}
		s.liveBindings[localID] = bind
	}
}

func (s *observationScope) resolve(id int) (int, error) {
	if id >= 0 {
		return id, nil
	}
	if actual := s.ids[id]; actual != 0 {
		return actual, nil
	}
	return 0, errObservationParentUnavailable
}

func (s *observationScope) submit(ctx context.Context, byteLength int64, run func(context.Context) error, freeze ...func()) error {
	return s.submitJob(ctx, byteLength, "", 0, run, freeze...)
}

func (s *observationScope) submitCore(ctx context.Context, id int, byteLength int64, run func(context.Context) error, freeze ...func()) error {
	return s.submitJob(ctx, byteLength, "core", id, run, freeze...)
}

func (s *observationScope) submitCorePayload(ctx context.Context, id int, byteLength int64, compact func() int64, run func(context.Context) error, freeze func()) error {
	if err := s.submitCore(ctx, id, byteLength, run, freeze); err == nil {
		return nil
	}
	byteLength = compact()
	if info := observationBodyInfoFromContext(ctx); info != nil {
		copy := *info
		copy.Unavailable = !copy.Omitted
		ctx = withObservationBodyInfo(ctx, &copy)
	}
	return s.submitCore(ctx, id, byteLength, run)
}

func (s *observationScope) submitUsage(ctx context.Context, byteLength int64, run func(context.Context) error) error {
	return s.submitJob(ctx, byteLength, "usage", 0, run)
}

func (s *observationScope) submitTerminal(ctx context.Context, id int, byteLength int64, run func(context.Context) error) error {
	return s.submitJob(ctx, byteLength, "terminal", id, run)
}

// Failed admission has not published the callback. Compact only optional bytes
// at that boundary, then consume the terminal credit reserved with the core.
func (s *observationScope) submitTerminalPayload(ctx context.Context, id int, byteLength int64, compact func() int64, run func(context.Context) error, freeze ...func()) error {
	if err := s.submitJob(ctx, byteLength, "terminal", id, run, freeze...); err == nil {
		return nil
	}
	byteLength = compact()
	if info := observationResponseBodyInfoFromContext(ctx); info != nil {
		copy := *info
		copy.Unavailable = !copy.Omitted
		ctx = withObservationResponseBodyInfo(ctx, &copy)
	}
	return s.submitTerminal(ctx, id, byteLength, run)
}

func (s *observationScope) submitJob(ctx context.Context, byteLength int64, kind string, id int, run func(context.Context) error, freeze ...func()) error {
	w := s.writer
	// Only the request-creation core snapshot needs routing profiles. In
	// particular, reserved terminal/usage credits must not carry them again.
	if kind != "core" {
		ctx = context.WithValue(ctx, requestObservationProfilesKey{}, false)
	}
	ctx = detachedObservationContext(ctx)
	if w.primary != nil {
		ctx = ent.NewContext(ctx, w.primary)
	}
	byteLength += observationContextBytes(ctx)
	// Fixed metadata/closure overhead is charged even for empty payloads.
	byteLength += 4096
	w.mu.Lock()
	core, usage, terminal := kind == "core", kind == "usage", kind == "terminal"
	reserved := core && s.reservedCore > 0 || usage && s.reservedUsage || terminal && s.terminalCredits[id]
	additionalItems, additionalBytes := 1, byteLength
	if reserved {
		additionalItems = 0
		additionalBytes -= 4096
		if usage {
			additionalBytes -= observationUsageReservationBytes - 4096
		}
		if terminal {
			additionalBytes -= observationTerminalReservationBytes - 4096
		}
	}
	if core && !reserved {
		additionalItems++
		additionalBytes += observationTerminalReservationBytes
	}
	if usage && s.usageSubmitted {
		w.mu.Unlock()
		return nil
	}
	if !w.started || w.stopping || s.ended || w.items+additionalItems > w.config.MaxItems || byteLength > int64(w.config.MaxBytesMiB)<<20 || w.bytes+additionalBytes > int64(w.config.MaxBytesMiB)<<20 {
		w.mu.Unlock()
		metrics.RecordManagedObservabilityAdmissionSkippedComponent(context.Background(), "async_capacity", "forwarding_observation")
		return errObservationQueueUnavailable
	}
	if w.items == 0 {
		w.idle = make(chan struct{})
	}
	w.items += additionalItems
	w.bytes += additionalBytes
	if reserved && core {
		s.reservedCore--
		s.reservedTerminal--
	}
	if core {
		s.terminalCredits[id] = true
	}
	if terminal && reserved {
		delete(s.terminalCredits, id)
	}
	if reserved && usage {
		s.reservedUsage = false
	}
	usageKey := 0
	var pending *observationPendingUsage
	if usage {
		s.usageSubmitted = true
		pending, _ = ctx.Value(observationPendingUsageKey{}).(*observationPendingUsage)
		if key, ok := contexts.GetAPIKey(ctx); ok && key != nil {
			usageKey = key.ID
		}
		if pending == nil {
			pending = &observationPendingUsage{apiKeyID: usageKey, createdAt: time.Now().UTC()}
		}
		w.pendingRecords[pending] = struct{}{}
	}
	w.mu.Unlock()

	// The in-flight job now owns one item and byteLength bytes independently of
	// the scope's remaining credits. End/Stop may reclaim credits or queued jobs,
	// but only this owner can release an unpublished capture, after freezing ends.
	published := false
	defer func() {
		if !published {
			w.mu.Lock()
			w.items--
			w.bytes -= byteLength
			delete(w.pendingRecords, pending)
			if w.items == 0 {
				close(w.idle)
			}
			w.mu.Unlock()
		}
	}()
	for _, capture := range freeze {
		capture()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping || s.ended {
		return errObservationQueueUnavailable
	}
	// Remove only the submission marker; keep captured authorization/project
	// values. The callback must invoke synchronous service methods on this context.
	ctx = context.WithValue(context.WithoutCancel(ctx), observationContextKey{}, (*observationScope)(nil))
	w.jobs <- observationJob{ctx: ctx, bytes: byteLength, run: run, usage: pending}
	published = true
	return nil
}

func EndForwardingObservation(ctx context.Context) {
	s := observationFromContext(ctx)
	if s == nil {
		return
	}
	s.liveMu.Lock()
	if !s.liveClosed {
		s.liveClosed = true
		for _, cleanup := range s.liveCleanup {
			cleanup()
		}
		s.liveCleanup = nil
		s.liveBindings = nil
	}
	s.liveMu.Unlock()
	w := s.writer
	w.mu.Lock()
	defer w.mu.Unlock()
	if s.ended {
		return
	}
	s.ended = true
	delete(w.scopes, s)
	release := s.reservedCore
	releaseBytes := int64(release) * 4096
	if s.reservedUsage {
		release++
		releaseBytes += observationUsageReservationBytes
	}
	terminals := s.reservedTerminal + len(s.terminalCredits)
	release += terminals
	releaseBytes += int64(terminals) * observationTerminalReservationBytes
	s.reservedCore = 0
	s.reservedTerminal = 0
	clear(s.terminalCredits)
	s.reservedUsage = false
	w.items -= release
	w.bytes -= releaseBytes
	if release > 0 && w.items == 0 {
		close(w.idle)
	}
}

// Wait is an explicit diagnostic/test drain; forwarding never calls it.
func (w *ForwardingObservationWriter) Wait(ctx context.Context) error {
	for {
		w.mu.Lock()
		idle := w.idle
		w.mu.Unlock()
		select {
		case <-idle:
			if w.payloadLane != nil {
				if err := w.payloadLane.Wait(ctx); err != nil {
					return err
				}
				w.mu.Lock()
				empty := w.items == 0
				w.mu.Unlock()
				if !empty {
					continue
				}
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
