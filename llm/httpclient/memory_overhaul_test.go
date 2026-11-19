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
	require.NotNil(t, meta.Request, "DoStream ResponseMetadata preserves bodyless Request metadata")
	require.Nil(t, meta.Request.Body, "DoStream ResponseMetadata must not pin outbound Request.Body")
	require.Nil(t, meta.RawRequest, "DoStream ResponseMetadata must not pin net/http.Request")
	// Verify GetBody on RawResponse.Request was atomically released without mutating http.Request.
	if meta.RawResponse != nil && meta.RawResponse.Request != nil && meta.RawResponse.Request.GetBody != nil {
		replayed, err := meta.RawResponse.Request.GetBody()
		require.NoError(t, err)
		n, _ := io.Copy(io.Discard, replayed)
		_ = replayed.Close()
		require.Zero(t, n, "released outboundBodyPayload must not retain request bytes after 2xx")
	}
}

func TestRedirect307And308BodyReplay(t *testing.T) {
	t.Parallel()

	payload := []byte(`{"model":"claude-test","prompt":"redirect-replay-payload"}`)
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var received []byte
			var hops int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hops++
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				if r.URL.Path == "/redirect" {
					w.Header().Set("Location", "/final")
					w.WriteHeader(code)
					return
				}
				received = body
				if r.Header.Get("Accept") == "text/event-stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			}))
			defer srv.Close()

			hc := NewHttpClient()
			resp, err := hc.Do(context.Background(), &Request{
				Method:      http.MethodPost,
				URL:         srv.URL + "/redirect",
				ContentType: "application/json",
				Body:        payload,
			})
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, payload, received)
			require.Equal(t, payload, resp.Body)
			require.Equal(t, 2, hops)

			// Also verify DoStream follows 307/308 with full body replay.
			received = nil
			hops = 0
			stream, err := hc.DoStream(context.Background(), &Request{
				Method:      http.MethodPost,
				URL:         srv.URL + "/redirect",
				ContentType: "application/json",
				Body:        payload,
			})
			require.NoError(t, err)
			defer stream.Close()
			require.True(t, stream.Next())
			require.Equal(t, payload, received)
			require.Equal(t, 2, hops)
		})
	}
}

func TestHTTP2EarlyResponseAndCancelRace(t *testing.T) {
	t.Parallel()

	// Real HTTP/2 server sending an immediate 429 or 200 SSE response while the
	// client is still uploading a 4 MiB body, exercising concurrent Read/Close.
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/early-429":
			// Respond >299 immediately without reading request body so HTTP/2 closes
			// the upload body concurrently with client writeLoop.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
		case "/early-200-sse":
			// Respond 200 immediately before draining upload body.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			_, _ = io.WriteString(w, "data: {\"ok\":true}\n\n")
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	largeBody := bytes.Repeat([]byte("a"), 4<<20)
	hc := NewHttpClientWithClient(srv.Client())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := hc.DoStream(context.Background(), &Request{
				Method:      http.MethodPost,
				URL:         srv.URL + "/early-429",
				ContentType: "application/json",
				Body:        largeBody,
			})
			require.Error(t, err)
		}()
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := hc.DoStream(ctx, &Request{
				Method:      http.MethodPost,
				URL:         srv.URL + "/early-200-sse",
				ContentType: "application/json",
				Body:        largeBody,
			})
			if err == nil {
				cancel()
				_ = stream.Close()
			}
		}()
	}
	wg.Wait()
}

func TestReadLimitedBodySizedTruncatedUnexpectedEOF(t *testing.T) {
	t.Parallel()

	_, err := readLimitedBodySized(bytes.NewReader([]byte("short")), httpRequestBodyReadLimit, 100)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	_, err = readLimitedBodySized(bytes.NewReader(nil), httpRequestBodyReadLimit, 100)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
