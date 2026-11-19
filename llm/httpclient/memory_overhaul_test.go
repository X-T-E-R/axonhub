package httpclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestZstdAndGzipPoolingAndCorruptRecovery(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte(`{"role":"user","content":"pooled decompression check"}`), 128)

	// Zstd pool reuse + corrupt frame must not poison subsequent decodes.
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	defer enc.Close()
	zstdWire := enc.EncodeAll(payload, nil)

	for range 4 {
		decoded, err := decodeZstdRequestBody(zstdWire, httpRequestBodyReadLimit)
		require.NoError(t, err)
		require.Equal(t, payload, decoded)
	}
	_, err = decodeZstdRequestBody([]byte("not-a-valid-zstd-frame"), httpRequestBodyReadLimit)
	require.Error(t, err)
	decoded, err := decodeZstdRequestBody(zstdWire, httpRequestBodyReadLimit)
	require.NoError(t, err)
	require.Equal(t, payload, decoded)

	// Gzip pool reuse + corrupt payload must not poison subsequent decodes.
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	_, err = gw.Write(payload)
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	gzWire := gzBuf.Bytes()

	for range 4 {
		out, err := decodeRequestBodyWithLimit(gzWire, http.Header{"Content-Encoding": {"gzip"}}, httpRequestBodyReadLimit)
		require.NoError(t, err)
		require.Equal(t, payload, out)
	}
	_, err = decodeRequestBodyWithLimit([]byte("corrupt-gzip"), http.Header{"Content-Encoding": {"gzip"}}, httpRequestBodyReadLimit)
	require.Error(t, err)
	out, err := decodeRequestBodyWithLimit(gzWire, http.Header{"Content-Encoding": {"gzip"}}, httpRequestBodyReadLimit)
	require.NoError(t, err)
	require.Equal(t, payload, out)
}

func TestDCZDictDecoderCacheConcurrencyAndEviction(t *testing.T) {
	// Exercise concurrent decode across > maxCachedDictDecoders unique dictionaries
	// to verify LRU eviction does not race with in-flight DecodeAll calls.
	const numDicts = maxCachedDictDecoders + 6
	type fixture struct {
		hash  [sha256.Size]byte
		dict  []byte
		wire  []byte
		plain []byte
	}
	fixtures := make([]fixture, numDicts)
	for i := range numDicts {
		dict := bytes.Repeat([]byte{byte('a' + (i % 26)), byte('0' + (i % 10)), ':', ' '}, 512)
		plain := append(append([]byte(`{"idx":`), byte('A'+(i%26))), bytes.Repeat(dict[:64], 16)...)
		h := sha256.Sum256(dict)
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, dict), zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true))
		require.NoError(t, err)
		wire := make([]byte, 0, len(requestDictionaryMagic)+sha256.Size+len(plain))
		wire = append(wire, requestDictionaryMagic[:]...)
		wire = append(wire, h[:]...)
		wire = enc.EncodeAll(plain, wire)
		enc.Close()
		fixtures[i] = fixture{hash: h, dict: dict, wire: wire, plain: plain}
	}

	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range numDicts * 2 {
				fx := fixtures[(worker+i)%numDicts]
				lookup := func(h [sha256.Size]byte) ([]byte, bool) {
					if h == fx.hash {
						return fx.dict, true
					}
					return nil, false
				}
				got, err := decodeDictionaryRequest(fx.wire, lookup)
				require.NoError(t, err)
				require.Equal(t, fx.plain, got)
			}
		}(g)
	}
	wg.Wait()

	dictDecoderCache.mu.Lock()
	cacheLen := len(dictDecoderCache.entries)
	dictDecoderCache.mu.Unlock()
	require.LessOrEqual(t, cacheLen, maxCachedDictDecoders)
}

func TestReadLimitedBodySizedAndPoolCapGuard(t *testing.T) {
	t.Parallel()

	// Exact Content-Length path allocates once without doubling.
	exactData := bytes.Repeat([]byte("x"), 1<<20)
	got, err := readLimitedBodySized(bytes.NewReader(exactData), httpRequestBodyReadLimit, int64(len(exactData)))
	require.NoError(t, err)
	require.Equal(t, exactData, got)
	require.LessOrEqual(t, cap(got), len(exactData)+64)

	// Unknown length 4 MiB spike must not return an oversized buffer to limitedBodyBufPool.
	spike := bytes.Repeat([]byte("S"), 4<<20)
	gotSpike, err := readLimitedBodySized(io.NopCloser(bytes.NewReader(spike)), httpRequestBodyReadLimit, -1)
	require.NoError(t, err)
	require.Equal(t, len(spike), len(gotSpike))

	buf := bytes.NewBuffer(make([]byte, 0, maxPooledBufferCap+1))
	putLimitedBodyBuf(buf)
	for range 8 {
		pooled := limitedBodyBufPool.Get().(*bytes.Buffer)
		require.LessOrEqual(t, pooled.Cap(), maxPooledBufferCap)
		putLimitedBodyBuf(pooled)
	}
}

func TestPeekRequestBodyAndReadHTTPRequestZeroCopyHandoff(t *testing.T) {
	t.Parallel()

	rawPayload := []byte(`{"model":"claude-3-5-sonnet","metadata":{"user_id":"sess_123"}}`)
	req := httptest.NewRequest(http.MethodPost, "http://example.test/v1/messages", bytes.NewReader(rawPayload))
	SetDecodedRequestBody(req, rawPayload)

	peeked, err := PeekRequestBody(req)
	require.NoError(t, err)
	require.Equal(t, rawPayload, peeked)

	genericReq, err := ReadHTTPRequest(req)
	require.NoError(t, err)
	require.Equal(t, rawPayload, genericReq.Body)
	require.Equal(t, http.NoBody, req.Body, "consumed decodedBodyReader must be cleared from http.Request.Body")
}

func TestResponseHeaderTimeoutAndDoStreamRequestClearing(t *testing.T) {
	t.Parallel()

	// Default timeout is set on both NewHttpClient and NewHttpClientWithProxy.
	c1 := NewHttpClient()
	tr1, ok := c1.GetNativeClient().Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, DefaultResponseHeaderTimeout, tr1.ResponseHeaderTimeout)

	c2 := NewHttpClientWithProxy(&ProxyConfig{Type: ProxyTypeDisabled})
	tr2, ok := c2.GetNativeClient().Transport.(*http.Transport)
	require.True(t, ok)
	require.Equal(t, DefaultResponseHeaderTimeout, tr2.ResponseHeaderTimeout)

	// Short custom ResponseHeaderTimeout aborts hung upstream before headers arrive.
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer hung.Close()

	fastTimeoutClient := NewHttpClient(WithResponseHeaderTimeout(80 * time.Millisecond))
	start := time.Now()
	_, err := fastTimeoutClient.DoStream(context.Background(), &Request{
		Method: http.MethodPost,
		URL:    hung.URL,
		Body:   []byte(`{"hello":"world"}`),
	})
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)

	// Successful DoStream clears Request/RawRequest from ResponseMetadata and rawResp.Request.
	sseSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	defer sseSrv.Close()

	stream, err := NewHttpClient().DoStream(context.Background(), &Request{
		Method: http.MethodPost,
		URL:    sseSrv.URL,
		Body:   []byte(`{"model":"test","stream":true}`),
	})
	require.NoError(t, err)
	defer stream.Close()

	rs, ok := stream.(ResponseStream)
	require.True(t, ok)
	meta := rs.ResponseMetadata()
	require.NotNil(t, meta)
	require.Equal(t, http.StatusOK, meta.StatusCode)
	require.Nil(t, meta.Request, "DoStream ResponseMetadata must not pin outbound Request")
	require.Nil(t, meta.RawRequest, "DoStream ResponseMetadata must not pin net/http.Request")
	require.Nil(t, meta.RawResponse.Request, "DoStream must clear RawResponse.Request")
}
