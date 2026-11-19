package httpclient

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	"github.com/samber/lo"
)

const (
	httpRequestBodyReadLimit = 32 << 20
	// maxPooledBufferCap caps temporary bytes.Buffer capacities returned to
	// sync.Pool so multi-MB spike requests never ratchet idle pool memory.
	maxPooledBufferCap = 256 << 10
)

var ErrHTTPRequestBodyTooLarge = errors.New("http request body exceeds size limit")

// outboundBodyPayload holds an immutable outbound request payload behind an
// atomic pointer so net/http's GetBody closure can replay the body across
// 307/308 redirects and pre-response transport retries, yet DoStream can
// release the multi-MB slice as soon as 2xx response headers arrive without
// mutating any http.Request fields owned by http.Transport.
type outboundBodyPayload struct {
	data atomic.Pointer[[]byte]
}

func newOutboundBodyPayload(b []byte) *outboundBodyPayload {
	p := &outboundBodyPayload{}
	p.data.Store(&b)
	return p
}

func (p *outboundBodyPayload) Release() {
	if p != nil {
		p.data.Store(nil)
	}
}

func (p *outboundBodyPayload) NewReader() io.ReadCloser {
	var b []byte
	if ptr := p.data.Load(); ptr != nil {
		b = *ptr
	}
	return &outboundBodyReader{data: b}
}

// outboundBodyReader implements io.ReadCloser for outbound HTTP uploads with
// safe concurrent Read/Close semantics as required by net/http (e.g. HTTP/2
// closing the request body on an early >299 response or cancellation while
// still uploading).
type outboundBodyReader struct {
	mu     sync.Mutex
	data   []byte
	offset int
}

func (r *outboundBodyReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *outboundBodyReader) Close() error {
	r.mu.Lock()
	r.data = nil
	r.offset = 0
	r.mu.Unlock()
	return nil
}

// decodedBodyReader wraps an already-decoded inbound request body so middleware
// and ReadHTTPRequest can share or take ownership of the byte slice without
// duplicating multi-MB payloads or keeping them pinned on http.Request.Body.
type decodedBodyReader struct {
	data   []byte
	offset int
}

func (r *decodedBodyReader) Read(p []byte) (int, error) {
	if r.offset >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += n
	return n, nil
}

func (r *decodedBodyReader) Close() error {
	r.data = nil
	r.offset = 0
	return nil
}

// SetDecodedRequestBody installs a decoded body on rawReq for zero-copy
// handoff to downstream middleware and ReadHTTPRequest.
func SetDecodedRequestBody(rawReq *http.Request, body []byte) {
	if rawReq == nil {
		return
	}
	rawReq.Body = &decodedBodyReader{data: body}
	rawReq.ContentLength = int64(len(body))
	rawReq.Header.Del("Content-Encoding")
	rawReq.Header.Del("Content-Length")
}

// PeekRequestBody returns the request body bytes for read-only inspection
// (such as trace extraction) without copying when already buffered, and
// restores rawReq.Body for zero-copy consumption by ReadHTTPRequest.
func PeekRequestBody(rawReq *http.Request) ([]byte, error) {
	if rawReq == nil || rawReq.Body == nil {
		return nil, nil
	}
	if dr, ok := rawReq.Body.(*decodedBodyReader); ok && dr.offset == 0 {
		return dr.data, nil
	}
	body, err := readLimitedBodySized(rawReq.Body, httpRequestBodyReadLimit, rawReq.ContentLength)
	if err != nil {
		return nil, err
	}
	rawReq.Body = &decodedBodyReader{data: body}
	rawReq.ContentLength = int64(len(body))
	return body, nil
}

var (
	limitedBodyBufPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(make([]byte, 0, 8<<10))
		},
	}

	gzipReaderPool = sync.Pool{}

	// plainZstdDecoderPool reuses dictionary-free zstd decoders across requests.
	// Pooled decoders MUST NOT be closed (Close terminates internal resources);
	// only Reset(nil) is allowed before Put. Dictionary decoders are never placed
	// in this pool.
	plainZstdDecoderPool = sync.Pool{}
)

func putLimitedBodyBuf(buf *bytes.Buffer) {
	if buf == nil || buf.Cap() > maxPooledBufferCap {
		return
	}
	buf.Reset()
	limitedBodyBufPool.Put(buf)
}

type pooledGzipReader struct {
	br bytes.Reader
	gr *gzip.Reader
}

func acquireGzipReader(body []byte) (*pooledGzipReader, error) {
	p, ok := gzipReaderPool.Get().(*pooledGzipReader)
	if !ok || p == nil {
		p = &pooledGzipReader{}
	}
	p.br.Reset(body)
	if p.gr == nil {
		gr, err := gzip.NewReader(&p.br)
		if err != nil {
			p.br.Reset(nil)
			return nil, err
		}
		p.gr = gr
		return p, nil
	}
	if err := p.gr.Reset(&p.br); err != nil {
		p.br.Reset(nil)
		return nil, err
	}
	return p, nil
}

func releaseGzipReader(p *pooledGzipReader) {
	if p == nil {
		return
	}
	if p.gr != nil {
		_ = p.gr.Close()
	}
	p.br.Reset(nil)
	gzipReaderPool.Put(p)
}

func acquirePlainZstdDecoder() (*zstd.Decoder, error) {
	if v := plainZstdDecoderPool.Get(); v != nil {
		return v.(*zstd.Decoder), nil
	}
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(httpRequestBodyReadLimit),
		zstd.WithDecoderMaxWindow(32<<20),
	)
}

func releasePlainZstdDecoder(dec *zstd.Decoder) {
	if dec == nil {
		return
	}
	if err := dec.Reset(nil); err == nil {
		plainZstdDecoderPool.Put(dec)
	}
}

func ReadHTTPRequest(rawReq *http.Request) (*Request, error) {
	req := &Request{
		Method:     rawReq.Method,
		URL:        rawReq.URL.String(),
		Path:       rawReq.URL.Path,
		Query:      rawReq.URL.Query(),
		Headers:    rawReq.Header,
		Body:       nil,
		Auth:       &AuthConfig{},
		RequestID:  "",
		ClientIP:   getClientIP(rawReq),
		RawRequest: rawReq,
	}

	body, err := ReadRequestBody(rawReq)
	if err != nil {
		return nil, err
	}

	req.Body = body

	return req, nil
}

// ReadRequestBody reads and decodes an HTTP request with the same wire and decoded size limits.
// Callers that inspect the body before ReadHTTPRequest must restore the returned bytes.
func ReadRequestBody(rawReq *http.Request) ([]byte, error) {
	return ReadRequestBodyWithDictionary(rawReq, nil)
}

// ReadRequestBodyWithDictionary additionally accepts the opt-in DCZ request extension.
// A lookup must only expose dictionaries owned by the authenticated request scope.
func ReadRequestBodyWithDictionary(rawReq *http.Request, lookup RequestDictionaryLookup) ([]byte, error) {
	encoding, err := RequestContentEncoding(rawReq.Header)
	if err != nil {
		return nil, err
	}
	if dr, ok := rawReq.Body.(*decodedBodyReader); ok && dr.offset == 0 && (encoding == "" || encoding == "identity") {
		body := dr.data
		dr.data = nil
		rawReq.Body = http.NoBody
		if int64(len(body)) > httpRequestBodyReadLimit {
			return nil, ErrHTTPRequestBodyTooLarge
		}
		return body, nil
	}
	body, err := readLimitedBodySized(rawReq.Body, httpRequestBodyReadLimit, rawReq.ContentLength)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}
	rawReq.Body = http.NoBody
	if encoding == "dcz" {
		decoded, err := decodeDictionaryRequest(body, lookup)
		if err != nil {
			return nil, err
		}
		rawReq.Header.Del("Content-Encoding")
		rawReq.Header.Del("Content-Length")
		return decoded, nil
	}
	if len(body) == 0 {
		return body, nil
	}
	return decodeRequestBody(body, rawReq.Header)
}

func decodeRequestBody(body []byte, headers http.Header) ([]byte, error) {
	return decodeRequestBodyWithLimit(body, headers, httpRequestBodyReadLimit)
}

// RequestContentEncoding accepts only a single content coding, regardless of whether
// a coding list is represented by repeated fields or a comma-separated field.
func RequestContentEncoding(headers http.Header) (string, error) {
	values := headers.Values("Content-Encoding")
	if len(values) == 0 {
		return "", nil
	}
	if len(values) != 1 || strings.Contains(values[0], ",") {
		return "", errors.New("multiple request content encodings are unsupported")
	}
	return strings.ToLower(strings.TrimSpace(values[0])), nil
}

func decodeRequestBodyWithLimit(body []byte, headers http.Header, limit int64) ([]byte, error) {
	encoding, err := RequestContentEncoding(headers)
	if err != nil {
		return nil, err
	}

	switch encoding {
	case "identity", "":
		return body, nil

	case "gzip", "x-gzip":
		pooled, err := acquireGzipReader(body)
		if err != nil {
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer releaseGzipReader(pooled)

		decoded, err := readLimitedBody(pooled.gr, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress gzip body: %w", err)
		}

		headers.Del("Content-Encoding")
		headers.Del("Content-Length")

		return decoded, nil

	case "deflate":
		// RFC 7230/2616 defines "deflate" as zlib (RFC 1950), but many clients send
		// raw DEFLATE (RFC 1951). Try zlib first, fall back to raw DEFLATE.
		decoded, err := decodeZlibOrFlate(body, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to decompress deflate body: %w", err)
		}

		headers.Del("Content-Encoding")
		headers.Del("Content-Length")

		return decoded, nil

	case "zstd":
		decoded, err := decodeZstdRequestBody(body, limit)
		if err != nil {
			return nil, fmt.Errorf("failed to decode zstd compressed body: %w", err)
		}

		headers.Del("Content-Encoding")
		headers.Del("Content-Length")

		return decoded, nil

	default:
		return nil, fmt.Errorf("unsupported content encoding: %s", encoding)
	}
}

// The complete compressed frame is already buffered by ReadRequestBody. DecodeAll
// avoids a second streaming history window and bytes.Buffer copy per request.
func decodeZstdRequestBody(body []byte, limit int64, extra ...zstd.DOption) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrHTTPRequestBodyTooLarge
	}
	if len(extra) == 0 && limit <= httpRequestBodyReadLimit {
		decoder, err := acquirePlainZstdDecoder()
		if err != nil {
			return nil, err
		}
		defer releasePlainZstdDecoder(decoder)

		decoded, err := decoder.DecodeAll(body, nil)
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) || int64(len(decoded)) > limit {
			return nil, ErrHTTPRequestBodyTooLarge
		}
		if err != nil {
			return nil, err
		}
		return decoded, nil
	}

	options := []zstd.DOption{
		zstd.WithDecoderConcurrency(1),
		// Production's decoded cap is 32 MiB. Keep the existing window allowance
		// independent of smaller limits used by callers/tests.
		zstd.WithDecoderMaxMemory(uint64(max(limit, httpRequestBodyReadLimit))),
		zstd.WithDecoderMaxWindow(32 << 20),
	}
	options = append(options, extra...)
	decoder, err := zstd.NewReader(nil, options...)
	if err != nil {
		return nil, err
	}
	defer decoder.Close()

	decoded, err := decoder.DecodeAll(body, nil)
	if errors.Is(err, zstd.ErrDecoderSizeExceeded) || int64(len(decoded)) > limit {
		return nil, ErrHTTPRequestBodyTooLarge
	}
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func readLimitedBody(reader io.Reader, limit int64) ([]byte, error) {
	return readLimitedBodySized(reader, limit, -1)
}

func readLimitedBodySized(reader io.Reader, limit, contentLength int64) ([]byte, error) {
	if reader == nil {
		return nil, nil
	}
	if limit <= 0 {
		return nil, ErrHTTPRequestBodyTooLarge
	}

	// Exact single allocation when Content-Length is known and within limit:
	// io.ReadFull avoids bytes.Buffer.ReadFrom's MinRead (512B) doubling copy
	// at EOF on multi-MB JSON payloads.
	if contentLength > 0 && contentLength <= limit {
		body := make([]byte, contentLength)
		_, err := io.ReadFull(reader, body)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		var extra [1]byte
		en, err := reader.Read(extra[:])
		if en == 0 && (err == nil || errors.Is(err, io.EOF)) {
			return body, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if int64(len(body))+int64(en) > limit {
			return nil, ErrHTTPRequestBodyTooLarge
		}
		buf := bytes.NewBuffer(body)
		buf.Write(extra[:en])
		remaining := limit - int64(buf.Len())
		more, err := buf.ReadFrom(io.LimitReader(reader, remaining+1))
		if err != nil {
			return nil, err
		}
		if more > remaining {
			return nil, ErrHTTPRequestBodyTooLarge
		}
		return buf.Bytes(), nil
	}

	buf := limitedBodyBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	n, err := buf.ReadFrom(io.LimitReader(reader, limit+1))
	if err != nil {
		putLimitedBodyBuf(buf)
		return nil, err
	}
	if n > limit {
		putLimitedBodyBuf(buf)
		return nil, ErrHTTPRequestBodyTooLarge
	}
	if buf.Cap() > maxPooledBufferCap {
		// Hand ownership directly to the caller and drop the oversized buffer
		// instead of returning it to the pool (anti-pool-pollution guard).
		return buf.Bytes(), nil
	}
	out := bytes.Clone(buf.Bytes())
	putLimitedBodyBuf(buf)
	return out, nil
}

func decodeZlibOrFlate(body []byte, limit int64) ([]byte, error) {
	reader, err := zlib.NewReader(bytes.NewReader(body))
	if err == nil {
		defer reader.Close()
		return readLimitedBody(reader, limit)
	}

	flateReader := flate.NewReader(bytes.NewReader(body))
	defer flateReader.Close()
	return readLimitedBody(flateReader, limit)
}

func getClientIP(req *http.Request) string {
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		if before, _, ok := strings.Cut(xff, ","); ok {
			return strings.TrimSpace(before)
		}

		return xff
	}

	if xri := req.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	if ip, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		return ip
	}

	return req.RemoteAddr
}

// IsHTTPStatusCodeRetryable checks if an HTTP status code is retryable.
// 4xx status codes are generally not retryable except for 429 (Too Many Requests).
// 5xx status codes are typically retryable.
func IsHTTPStatusCodeRetryable(statusCode int) bool {
	if statusCode == http.StatusTooManyRequests {
		return true // 429 is retryable (rate limiting)
	}

	if statusCode >= 400 && statusCode < 500 {
		return false // Other 4xx errors are not retryable
	}

	if statusCode >= 500 {
		return true // 5xx errors are retryable
	}

	return false // Non-error status codes don't need retrying
}

// The golang std http client will handle the headers automatically.
var libManagedHeaders = map[string]bool{
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Accept-Encoding":   true,
	"Host":              true,
}

var blockedHeaders = map[string]bool{
	"Content-Type": true,
	// Accept is owned by the outbound transformer (e.g. TTS requires */* for binary
	// audio); the inbound client value must not override it.
	"Accept":             true,
	"Connection":         true,
	"Keep-Alive":         true,
	"Proxy-Authenticate": true,
	"Proxy-Connection":   true,
	"Te":                 true,
	"Trailer":            true,
	"Upgrade":            true,
	"X-Channel-Id":       true,
	"X-Project-Id":       true,
	"X-Real-Ip":          true,
	"X-Forwarded-For":    true,
	"X-Forwarded-Proto":  true,
	"X-Forwarded-Host":   true,
	"X-Forwarded-Port":   true,
	"X-Forwarded-Server": true,

	// Browser-only / hop-by-hop-ish headers that should not be forwarded to upstream.
	"Accept-Language":    true,
	"Dnt":                true,
	"Origin":             true,
	"Referer":            true,
	"Sec-Fetch-Dest":     true,
	"Sec-Fetch-Mode":     true,
	"Sec-Fetch-Site":     true,
	"Sec-Fetch-User":     true,
	"Sec-Ch-Ua":          true,
	"Sec-Ch-Ua-Mobile":   true,
	"Sec-Ch-Ua-Platform": true,

	// AxonHub customized headers that should not be forwarded to upstream to avoid recognition.
	// NOTE: user customized trace/thread headers will be sent to upstream.
	"Ah-Trace-Id":   true,
	"Ah-Thread-Id":  true,
	"Ah-Request-Id": true,

	// X-Initiator is used by specific channels (e.g. Copilot) for billing control.
	// Block from auto-merge so it is only forwarded by the channel that explicitly needs it.
	"X-Initiator": true,
}

// blockedHeaderPrefixes lists header prefixes that should not be forwarded to upstream.
// For example, Cloudflare adds Cf-* and Cdn-* headers that can cause upstream providers to reject requests.
var blockedHeaderPrefixes = []string{
	"Cf-",
	"Cdn-",
}

// isBlockedHeader checks whether a header (in canonical form) should be blocked from forwarding.
func isBlockedHeader(key string) bool {
	if blockedHeaders[key] {
		return true
	}

	for _, prefix := range blockedHeaderPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}

	return false
}

var sensitiveHeaders = map[string]bool{
	"Authorization":       true,
	"Api-Key":             true,
	"X-Api-Key":           true,
	"X-Api-Secret":        true,
	"X-Api-Token":         true,
	"X-Goog-Api-Key":      true,
	"X-Google-Api-Key":    true,
	"Cookie":              true,
	"Set-Cookie":          true,
	"Proxy-Authorization": true,
	"Www-Authenticate":    true,
}

func IsSensitiveHeader(key string) bool {
	return sensitiveHeaders[http.CanonicalHeaderKey(key)] || strings.HasPrefix(http.CanonicalHeaderKey(key), "Cf-Access-")
}

var mergeWithAppendHeaders = map[string]bool{}

// RegisterMergeWithAppendHeaders registers headers that should be appended instead of overwritten.
// It is not goroutine-safe, should call when init.
func RegisterMergeWithAppendHeaders(headers ...string) {
	for _, h := range headers {
		mergeWithAppendHeaders[http.CanonicalHeaderKey(h)] = true
	}
}

func MergeInboundRequest(dest, src *Request) *Request {
	if src == nil || len(src.Headers) == 0 && len(src.Query) == 0 {
		return dest
	}

	dest.Headers = MergeHTTPHeaders(dest.Headers, src.Headers)

	if !dest.SkipInboundQueryMerge {
		dest.Query = MergeHTTPQuery(dest.Query, src.Query)
	}

	return dest
}

// MergeHTTPQuery merges the source query parameters into the destination query parameters.
// If a key already exists in the destination, it is not overwritten.
func MergeHTTPQuery(dest, src url.Values) url.Values {
	if len(src) == 0 {
		return dest
	}

	if dest == nil {
		dest = make(url.Values)
	}

	for k, v := range src {
		if _, ok := dest[k]; !ok {
			dest[k] = v
		}
	}

	return dest
}

func MaskSensitiveHeaders(headers http.Header) http.Header {
	result := make(http.Header, len(headers))
	for key, values := range headers {
		var newValues []string
		if !IsSensitiveHeader(key) {
			newValues = values
		} else {
			newValues = append(newValues, "******")
		}

		result[key] = newValues
	}

	return result
}

// FinalizeAuthHeaders writes the auth config into headers and clears the in-memory auth field.
func FinalizeAuthHeaders(req *Request) (*Request, error) {
	if req.Auth == nil {
		return req, nil
	}

	err := applyAuth(req.Headers, req.Auth)
	if err != nil {
		return nil, fmt.Errorf("failed to apply authentication: %w", err)
	}

	req.Auth = nil

	return req, nil
}

// MergeHTTPHeaders merges the source headers into the destination headers.
// If a header is in the mergeWithAppendHeaders list, it adds non-duplicate values from the source.
// Otherwise, it overwrites the destination header with the source values.
// Blocked, sensitive, and library-managed headers are not merged.
func MergeHTTPHeaders(dest, src http.Header) http.Header {
	for k, v := range src {
		if IsSensitiveHeader(k) || libManagedHeaders[k] || isBlockedHeader(k) {
			continue
		}

		if mergeWithAppendHeaders[k] {
			if existingValues, ok := dest[k]; ok {
				dest[k] = lo.Uniq(append(existingValues, v...))
			} else {
				dest[k] = v
			}
		} else {
			dest[k] = v
		}
	}

	return dest
}
