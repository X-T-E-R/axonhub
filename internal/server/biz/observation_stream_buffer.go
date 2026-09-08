package biz

import (
	"context"
	"sync"

	"github.com/looplj/axonhub/llm/httpclient"
)

// JSON escaping may use six bytes per source byte. The remaining two shares
// cover retained events and decoded aggregation while the result is encoded.
const observationStreamWorkingSetFactor = 8

// ObservationStreamBuffer accounts live optional evidence against the same
// process budget as queued work. Business stream transformation never uses it.
type ObservationStreamBuffer struct {
	appendMu      sync.Mutex
	writer        *ForwardingObservationWriter
	bytes         int64
	BodyEnabled   bool
	ChunksEnabled bool
	Unavailable   bool
	chunks        []*httpclient.StreamEvent
	closed        bool
}

func (s *RequestService) NewObservationStreamBuffer(ctx context.Context, channel *Channel, execution bool) *ObservationStreamBuffer {
	scope := observationFromContext(ctx)
	if scope == nil {
		return nil
	}
	policy := s.SystemService.StoragePolicyOrDefault(ctx)
	b := &ObservationStreamBuffer{BodyEnabled: policy.StoreResponseBody, ChunksEnabled: policy.StoreChunks}
	if execution {
		b.BodyEnabled = s.shouldStoreExecutionResponseBody(ctx, nil, channel)
		b.ChunksEnabled = s.shouldStoreExecutionStreamChunks(ctx, nil, channel)
	}
	b.writer = scope.writer
	scope.liveMu.Lock()
	if scope.liveClosed {
		b.Unavailable = true
	} else {
		scope.liveCleanup = append(scope.liveCleanup, b.Close)
	}
	scope.liveMu.Unlock()
	return b
}

func (b *ObservationStreamBuffer) Append(event *httpclient.StreamEvent) []*httpclient.StreamEvent {
	if event == nil || (!b.BodyEnabled && !b.ChunksEnabled) {
		return nil
	}
	// Only appends on this stream serialize. Close and other streams do not
	// wait for a potentially large clone or slice growth.
	b.appendMu.Lock()
	defer b.appendMu.Unlock()
	b.writer.mu.Lock()
	if b.Unavailable || b.closed {
		b.writer.mu.Unlock()
		return nil
	}
	// Charge the event object/slice capacity as well as data. Reserve headroom
	// for aggregation and its JSON representation before retaining the event.
	size := observationStreamWorkingSetFactor * int64(len(event.Data)+len(event.Type)+len(event.LastEventID)+128)
	limit := int64(b.writer.config.MaxBytesMiB) << 20
	if !b.writer.started || b.writer.stopping || b.writer.bytes+size > limit || b.writer.optionalBytes+size > b.writer.optionalByteLimit() {
		b.writer.bytes -= b.bytes
		b.writer.optionalBytes -= b.bytes
		b.bytes, b.chunks, b.Unavailable = 0, nil, true
		b.writer.mu.Unlock()
		return nil
	}
	b.writer.bytes += size
	b.writer.optionalBytes += size
	ownedBytes, chunks := b.bytes+size, b.chunks
	// This append owns the previous capture plus the new reservation until it
	// publishes or discards them. Concurrent Close cannot return those bytes.
	b.bytes, b.chunks = 0, nil
	b.writer.mu.Unlock()
	published := false
	defer func() {
		if !published {
			b.writer.mu.Lock()
			b.writer.bytes -= ownedBytes
			b.writer.optionalBytes -= ownedBytes
			b.writer.mu.Unlock()
		}
	}()
	chunks = append(chunks, cloneObservationStreamChunks([]*httpclient.StreamEvent{event})[0])
	b.writer.mu.Lock()
	defer b.writer.mu.Unlock()
	if b.closed || b.writer.stopping {
		return nil
	}
	b.bytes, b.chunks = ownedBytes, chunks
	published = true
	return b.chunks
}

func (b *ObservationStreamBuffer) Close() {
	if b == nil {
		return
	}
	b.writer.mu.Lock()
	defer b.writer.mu.Unlock()
	b.writer.bytes -= b.bytes
	b.writer.optionalBytes -= b.bytes
	b.bytes, b.chunks = 0, nil
	b.closed = true
}

func (b *ObservationStreamBuffer) Context(ctx context.Context) context.Context {
	if b == nil {
		return ctx
	}
	if !b.BodyEnabled || b.Unavailable {
		ctx = withObservationResponseBodyInfo(ctx, &requestObservationBodyInfo{Omitted: !b.BodyEnabled, Unavailable: b.BodyEnabled && b.Unavailable})
	}
	if !b.ChunksEnabled || b.Unavailable {
		ctx = context.WithValue(ctx, requestObservationChunksInfoKey{}, &requestObservationBodyInfo{Omitted: !b.ChunksEnabled, Unavailable: b.ChunksEnabled && b.Unavailable})
	}
	return ctx
}
