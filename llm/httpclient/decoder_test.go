package httpclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"net"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// mockStreamDecoder implements StreamDecoder for testing.
type mockStreamDecoder struct {
	rc     io.ReadCloser
	events []*StreamEvent
	index  int
	err    error
	closed bool
}

func newMockStreamDecoder(ctx context.Context, rc io.ReadCloser, events []*StreamEvent) *mockStreamDecoder {
	return &mockStreamDecoder{
		rc:     rc,
		events: events,
		index:  -1,
	}
}

func (m *mockStreamDecoder) Next() bool {
	if m.err != nil {
		return false
	}

	m.index++

	return m.index < len(m.events)
}

func (m *mockStreamDecoder) Current() *StreamEvent {
	if m.index < 0 || m.index >= len(m.events) {
		return nil
	}

	return m.events[m.index]
}

func (m *mockStreamDecoder) Err() error {
	return m.err
}

func (m *mockStreamDecoder) Close() error {
	m.closed = true
	return m.rc.Close()
}

// mockReadCloser for testing.
type mockReadCloser struct {
	*bytes.Reader

	closed bool
}

func (m *mockReadCloser) Close() error {
	m.closed = true
	return nil
}

func newMockReadCloser(data []byte) *mockReadCloser {
	return &mockReadCloser{
		Reader: bytes.NewReader(data),
		closed: false,
	}
}

type readChunkThenError struct {
	data []byte
	err  error
	read bool
}

func (r *readChunkThenError) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}

	r.read = true
	n := copy(p, r.data)

	return n, r.err
}

func (r *readChunkThenError) Close() error {
	return nil
}

type cancelThenErrorReadCloser struct {
	cancel context.CancelFunc
	err    error
}

type closeUnblocksWithErrorReadCloser struct {
	err         error
	readStarted chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

func newCloseUnblocksWithErrorReadCloser(err error) *closeUnblocksWithErrorReadCloser {
	return &closeUnblocksWithErrorReadCloser{
		err:         err,
		readStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (r *closeUnblocksWithErrorReadCloser) Read([]byte) (int, error) {
	r.startOnce.Do(func() { close(r.readStarted) })
	<-r.release

	return 0, r.err
}

func (r *closeUnblocksWithErrorReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.release) })

	return nil
}

func (r *cancelThenErrorReadCloser) Read([]byte) (int, error) {
	r.cancel()
	return 0, r.err
}

func (r *cancelThenErrorReadCloser) Close() error {
	return nil
}

func TestRegisterDecoder(t *testing.T) {
	// Save original state
	originalDecoders := make(map[string]StreamDecoderFactory)
	maps.Copy(originalDecoders, globalRegistry.decoders)

	// Clean up after test
	defer func() {
		globalRegistry.mu.Lock()
		globalRegistry.decoders = originalDecoders
		globalRegistry.mu.Unlock()
	}()

	// Test registering a new decoder
	testContentType := "application/test"
	testFactory := func(ctx context.Context, rc io.ReadCloser) StreamDecoder {
		return newMockStreamDecoder(ctx, rc, []*StreamEvent{})
	}

	RegisterDecoder(testContentType, testFactory)

	// Verify decoder was registered
	factory, exists := GetDecoder(testContentType)
	require.True(t, exists)
	require.NotNil(t, factory)

	// Test that the factory works
	ctx := context.Background()
	rc := newMockReadCloser([]byte("test"))
	decoder := factory(ctx, rc)
	require.NotNil(t, decoder)
	require.Implements(t, (*StreamDecoder)(nil), decoder)
}

func TestGetDecoder(t *testing.T) {
	// Test getting existing decoder (text/event-stream should be registered by default)
	factory, exists := GetDecoder("text/event-stream")
	require.True(t, exists)
	require.NotNil(t, factory)

	// Test getting non-existent decoder
	factory, exists = GetDecoder("application/non-existent")
	require.False(t, exists)
	require.Nil(t, factory)
}

func TestDefaultSSEDecoder(t *testing.T) {
	// Create a simple SSE stream
	sseData := "data: {\"type\": \"test\", \"message\": \"hello\"}\n\n"
	rc := newMockReadCloser([]byte(sseData))

	// Create decoder
	ctx := context.Background()
	decoder := NewDefaultSSEDecoder(ctx, rc)
	require.NotNil(t, decoder)
	require.Implements(t, (*StreamDecoder)(nil), decoder)

	// Test Next() and Current()
	hasNext := decoder.Next()
	require.True(t, hasNext)
	require.NoError(t, decoder.Err())

	event := decoder.Current()
	require.NotNil(t, event)
	require.Equal(t, "", event.Type) // Default SSE type
	require.Contains(t, string(event.Data), "hello")

	// Test Close()
	err := decoder.Close()
	require.NoError(t, err)
	require.True(t, rc.closed)
}

func TestDefaultSSEDecoder_EmptyStream(t *testing.T) {
	ctx := context.Background()
	rc := newMockReadCloser([]byte(""))
	decoder := NewDefaultSSEDecoder(ctx, rc)

	// Should return false for empty stream
	hasNext := decoder.Next()
	require.False(t, hasNext)

	// Current should return nil
	event := decoder.Current()
	require.Nil(t, event)

	// Close should work
	err := decoder.Close()
	require.NoError(t, err)
}

func TestDefaultSSEDecoder_NextAfterClose(t *testing.T) {
	// Create a simple SSE stream
	sseData := "data: {\"type\": \"test\", \"message\": \"hello\"}\n\n"
	rc := newMockReadCloser([]byte(sseData))

	ctx := context.Background()
	decoder := NewDefaultSSEDecoder(ctx, rc)

	err := decoder.Close()
	require.NoError(t, err)
	require.True(t, rc.closed)

	hasNext := decoder.Next()
	require.False(t, hasNext)
	require.NoError(t, decoder.Err())
}

func TestDefaultSSEDecoder_PreservesTransportErrorWhenContextIsCanceledLater(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transportErr := errors.New("upstream connection reset")
	decoder := NewDefaultSSEDecoder(ctx, &cancelThenErrorReadCloser{
		cancel: cancel,
		err:    transportErr,
	})

	require.False(t, decoder.Next())
	require.ErrorIs(t, decoder.Err(), transportErr)
	require.NotErrorIs(t, decoder.Err(), context.Canceled)
	require.NoError(t, decoder.Close())
	require.ErrorIs(t, decoder.Err(), transportErr, "later Close must not clear an established provider error")
}

func TestDefaultSSEDecoder_GenuineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decoder := NewDefaultSSEDecoder(ctx, newMockReadCloser(nil))

	require.False(t, decoder.Next())
	require.ErrorIs(t, decoder.Err(), context.Canceled)
}

func TestDefaultSSEDecoder_LocalInterruptSuppressesReadErrorCausedByClose(t *testing.T) {
	transportErr := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: net.ErrClosed,
	}
	rc := newCloseUnblocksWithErrorReadCloser(transportErr)
	decoder := NewDefaultSSEDecoder(context.Background(), rc)
	interruptible := decoder.(interface{ Interrupt() error })

	nextReturned := make(chan bool, 1)
	go func() { nextReturned <- decoder.Next() }()

	<-rc.readStarted
	require.NoError(t, interruptible.Interrupt())
	require.False(t, <-nextReturned)
	require.NoError(t, decoder.Err(),
		"a read error caused by the decoder's own interrupt is teardown evidence, not an upstream failure")
}

func TestDefaultSSEDecoder_InterruptAfterReadCompletionDoesNotSuppressProviderError(t *testing.T) {
	transportErr := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: net.ErrClosed,
	}
	decoder := NewDefaultSSEDecoder(context.Background(), &cancelThenErrorReadCloser{
		cancel: func() {},
		err:    transportErr,
	})
	concreteDecoder := decoder.(*defaultSSEDecoder)
	interruptible := decoder.(interface{ Interrupt() error })
	recvReturned := make(chan struct{})
	classify := make(chan struct{})
	concreteDecoder.afterRecv = func() {
		close(recvReturned)
		<-classify
	}

	nextReturned := make(chan bool, 1)
	go func() { nextReturned <- decoder.Next() }()

	<-recvReturned
	require.NoError(t, interruptible.Interrupt(),
		"the read has completed, but Interrupt occurs before Next classifies its provider error")
	close(classify)
	require.False(t, <-nextReturned)
	require.ErrorIs(t, decoder.Err(), transportErr,
		"a later local Close must not relabel or clear an already completed provider error")
}

func TestBinaryChunkDecoder(t *testing.T) {
	ctx := context.Background()
	rc := newMockReadCloser([]byte("abcdefgh"))
	decoder := NewBinaryChunkDecoder("audio/mpeg")(ctx, rc)

	require.True(t, decoder.Next())
	first := decoder.Current()
	require.NotNil(t, first)
	require.Equal(t, "audio/mpeg", first.Type)
	require.Equal(t, []byte("abcdefgh"), first.Data)

	require.True(t, decoder.Next())
	done := decoder.Current()
	require.NotNil(t, done)
	require.Equal(t, BinaryStreamDoneEventType, done.Type)
	require.Empty(t, done.Data)

	require.False(t, decoder.Next())
	require.NoError(t, decoder.Err())
	require.NoError(t, decoder.Close())
	require.True(t, rc.closed)
}

func TestBinaryChunkDecoder_PreservesReadErrorAfterChunk(t *testing.T) {
	ctx := context.Background()
	streamErr := errors.New("truncated audio stream")
	decoder := NewBinaryChunkDecoder("audio/mpeg")(ctx, &readChunkThenError{
		data: []byte("abc"),
		err:  streamErr,
	})

	require.True(t, decoder.Next())
	chunk := decoder.Current()
	require.NotNil(t, chunk)
	require.Equal(t, "audio/mpeg", chunk.Type)
	require.Equal(t, []byte("abc"), chunk.Data)
	require.NoError(t, decoder.Err())

	require.False(t, decoder.Next())
	require.ErrorIs(t, decoder.Err(), streamErr)
}

func TestBinaryChunkDecoder_PreservesTransportErrorWhenContextIsCanceledLater(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	transportErr := errors.New("truncated upstream audio")
	decoder := NewBinaryChunkDecoder("audio/mpeg")(ctx, &cancelThenErrorReadCloser{
		cancel: cancel,
		err:    transportErr,
	})

	require.False(t, decoder.Next())
	require.ErrorIs(t, decoder.Err(), transportErr)
	require.NotErrorIs(t, decoder.Err(), context.Canceled)
}

func TestBinaryChunkDecoder_GenuineCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	decoder := NewBinaryChunkDecoder("audio/mpeg")(ctx, newMockReadCloser(nil))

	require.False(t, decoder.Next())
	require.ErrorIs(t, decoder.Err(), context.Canceled)
}

func TestStreamDecoderInterface(t *testing.T) {
	ctx := context.Background()
	// Test that our mock decoder implements the interface correctly
	events := []*StreamEvent{
		{Type: "test1", Data: []byte("data1")},
		{Type: "test2", Data: []byte("data2")},
	}

	rc := newMockReadCloser([]byte("test"))
	decoder := newMockStreamDecoder(ctx, rc, events)

	// Test Next() and Current() for multiple events
	require.True(t, decoder.Next())
	event1 := decoder.Current()
	require.Equal(t, "test1", event1.Type)
	require.Equal(t, []byte("data1"), event1.Data)

	require.True(t, decoder.Next())
	event2 := decoder.Current()
	require.Equal(t, "test2", event2.Type)
	require.Equal(t, []byte("data2"), event2.Data)

	// No more events
	require.False(t, decoder.Next())
	require.Nil(t, decoder.Current())

	// Test error handling
	require.NoError(t, decoder.Err())

	// Test close
	err := decoder.Close()
	require.NoError(t, err)
	require.True(t, decoder.closed)
	require.True(t, rc.closed)
}
