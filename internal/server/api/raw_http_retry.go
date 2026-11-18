package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	rawHTTPRetryBodyLimit = 2 << 20
	rawHTTPMaxAttempts    = 3
)

// rawHTTPRetryTransport keeps Raw HTTP retries inside ReverseProxy's transport
// boundary, so only responses that have not committed downstream headers can
// move to another credential.
type rawHTTPRetryTransport struct {
	channels       *biz.ChannelService
	channel        *biz.Channel
	defaultClient  *httpclient.HttpClient
	authType       string
	authHeader     string
	initialKey     string
	maxAttempts    int
	disableRetries bool

	mu         sync.Mutex
	transports []*http.Transport
}

func (t *rawHTTPRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	maxAttempts := t.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	if maxAttempts > rawHTTPMaxAttempts {
		maxAttempts = rawHTTPMaxAttempts
	}

	var body []byte
	canReplay := req.Body == nil || req.Body == http.NoBody
	if !t.disableRetries && maxAttempts > 1 {
		body, canReplay = bufferRawHTTPRetryBody(req)
	}
	if t.disableRetries || !canReplay {
		maxAttempts = 1
	}

	key := t.initialKey
	if selected, ok := contexts.GetChannelAPIKey(ctx); ok && selected != "" {
		key = selected
	}
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			t.finishAttempt(ctx, key, 0, true, true, time.Now())
			return nil, err
		}

		attemptReq := req
		if maxAttempts > 1 {
			attemptReq = req.Clone(ctx)
			attemptReq.Header = req.Header.Clone()
			if req.Body != nil && req.Body != http.NoBody {
				attemptReq.Body = io.NopCloser(bytes.NewReader(body))
			}
			attemptReq.GetBody = nil
		}
		setRawHTTPAuth(attemptReq.Header, t.authType, t.authHeader, key)

		transport, closeTransport, err := t.transportForCredential(ctx)
		if err != nil {
			t.finishAttempt(ctx, key, 0, true, ctx.Err() != nil, time.Now())
			return nil, err
		}

		informational := bufferRawHTTPInformational(attemptReq)
		attemptStarted := time.Now()
		resp, roundTripErr := transport.RoundTrip(attemptReq)
		if attemptReq != req && attemptReq.Body != nil {
			_ = attemptReq.Body.Close()
		}
		if roundTripErr != nil && resp != nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			resp = nil
		}

		canceled := ctx.Err() != nil || errors.Is(roundTripErr, context.Canceled) || errors.Is(roundTripErr, context.DeadlineExceeded)
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		retryable := !canceled && canReplay && attempt < maxAttempts &&
			((roundTripErr != nil && resp == nil) || (resp != nil && retryableRawHTTPStatus(resp.StatusCode)))

		if retryable {
			// Resolve the next credential while the last upstream response is still
			// open. If selection is exhausted, that response remains available to
			// ReverseProxy unchanged.
			t.finishAttempt(ctx, key, status, true, false, attemptStarted)
			contexts.ExcludeChannelCredential(ctx, t.channel.ID, key)
			nextKey := t.channels.RawHTTPAPIKey(ctx, t.channel)
			if nextKey != "" && nextKey != key {
				informational.discard()
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if closeTransport {
					transport.CloseIdleConnections()
				}
				key = nextKey
				continue
			}
			informational.discard()
			if resp != nil {
				return resp, nil
			}
			return nil, roundTripErr
		}

		if roundTripErr != nil {
			informational.discard()
			t.finishAttempt(ctx, key, 0, !canceled, canceled, attemptStarted)
			return nil, roundTripErr
		}
		if resp == nil {
			informational.discard()
			t.finishAttempt(ctx, key, http.StatusBadGateway, true, false, attemptStarted)
			return nil, errors.New("raw HTTP transport returned no response")
		}

		if resp.StatusCode >= 400 || canceled {
			informational.discard()
		} else {
			informational.commit()
		}
		if resp.Body == nil {
			t.finishAttempt(ctx, key, resp.StatusCode, rawHTTPFailureStatus(resp.StatusCode), canceled, attemptStarted)
			resp.Body = http.NoBody
		} else {
			resp.Body = &rawHTTPAttemptResponseBody{
				ReadCloser: resp.Body,
				finish: func(readErr error) {
					failed := rawHTTPFailureStatus(resp.StatusCode) || (readErr != nil && readErr != io.EOF)
					finishStatus := resp.StatusCode
					if failed && finishStatus < 400 {
						finishStatus = http.StatusBadGateway
					}
					t.finishAttempt(ctx, key, finishStatus, failed, ctx.Err() != nil, attemptStarted)
				},
			}
		}
		return resp, nil
	}

	return nil, errors.New("raw HTTP retry budget exhausted")
}

func (t *rawHTTPRetryTransport) transportForCredential(ctx context.Context) (*http.Transport, bool, error) {
	if transport, ok := contexts.GetChannelCredentialRawTransport(ctx); ok {
		return transport, false, nil
	}
	client := t.defaultClient
	if egress, ok := contexts.GetChannelCredentialEgress(ctx); ok {
		client = egress
	}
	if client == nil || client.GetNativeClient() == nil {
		return nil, false, errors.New("raw HTTP channel transport unavailable")
	}

	base := client.GetNativeClient().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	transport, ok := base.(*http.Transport)
	if !ok {
		return nil, false, errors.New("raw HTTP channel transport must be an HTTP transport")
	}
	clone := transport.Clone()
	clone.DisableCompression = true
	t.mu.Lock()
	t.transports = append(t.transports, clone)
	t.mu.Unlock()
	return clone, true, nil
}

func (t *rawHTTPRetryTransport) CloseIdleConnections() {
	t.mu.Lock()
	transports := slices.Clone(t.transports)
	t.transports = nil
	t.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func (t *rawHTTPRetryTransport) finishAttempt(ctx context.Context, key string, status int, failed, canceled bool, started time.Time) {
	contexts.FinishChannelCredentialAttempt(ctx, status, failed, canceled)
	if !failed || canceled {
		return
	}
	policyStatus := status
	if policyStatus == 0 {
		policyStatus = http.StatusBadGateway
	}
	perf := &biz.PerformanceRecord{
		ChannelID:          t.channel.ID,
		CredentialOwnerID:  t.channel.CredentialOwnerID(),
		APIKey:             key,
		StartTime:          started,
		EndTime:            time.Now(),
		ResponseStatusCode: policyStatus,
		RequestCompleted:   true,
	}
	t.channels.ApplyRequestFailurePolicy(ctx, perf)
}

func setRawHTTPAuth(header http.Header, authType, authHeader, key string) {
	for _, name := range middleware.DefaultAPIKeyConfig.Headers {
		header.Del(name)
	}
	if authHeader != "" {
		header.Del(authHeader)
	}
	if authType == "bearer" {
		header.Set("Authorization", "Bearer "+key)
		return
	}
	header.Set(authHeader, key)
}

func rawHTTPFailureStatus(status int) bool {
	return status >= 400
}

func retryableRawHTTPStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests,
		432, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func bufferRawHTTPRetryBody(req *http.Request) ([]byte, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true
	}
	if req.ContentLength > rawHTTPRetryBodyLimit {
		return nil, false
	}

	original := req.Body
	body, readErr := io.ReadAll(io.LimitReader(original, rawHTTPRetryBodyLimit+1))
	if len(body) > rawHTTPRetryBodyLimit {
		req.Body = &rawHTTPRestoredBody{
			Reader: io.MultiReader(bytes.NewReader(body), original),
			closer: original,
		}
		return nil, false
	}
	if readErr != nil {
		req.Body = &rawHTTPRestoredBody{
			Reader: io.MultiReader(bytes.NewReader(body), rawHTTPBodyError{err: readErr}),
			closer: original,
		}
		return nil, false
	}
	if req.ContentLength >= 0 && int64(len(body)) != req.ContentLength {
		req.Body = &rawHTTPRestoredBody{Reader: bytes.NewReader(body), closer: original}
		return nil, false
	}
	_ = original.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

type rawHTTPBodyError struct{ err error }

func (r rawHTTPBodyError) Read([]byte) (int, error) { return 0, r.err }

type rawHTTPRestoredBody struct {
	io.Reader
	closer io.Closer
	once   sync.Once
}

func (b *rawHTTPRestoredBody) Close() error {
	var err error
	b.once.Do(func() { err = b.closer.Close() })
	return err
}

type rawHTTPAttemptResponseBody struct {
	io.ReadCloser
	finish func(error)
	once   sync.Once
}

func (b *rawHTTPAttemptResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.finishOnce(err)
	}
	return n, err
}

func (b *rawHTTPAttemptResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.finishOnce(err)
	return err
}

func (b *rawHTTPAttemptResponseBody) finishOnce(err error) {
	b.once.Do(func() { b.finish(err) })
}

type rawHTTPInformationalResponse struct {
	status int
	header textproto.MIMEHeader
}

// ReverseProxy's default Got1xxResponse callback writes informational headers
// as soon as they arrive. Buffer it so a credential rejected before final headers
// cannot commit a 100/103 response downstream before a possible credential retry.
type rawHTTPInformationalBuffer struct {
	trace    *httptrace.ClientTrace
	original func(int, textproto.MIMEHeader) error
	mu       sync.Mutex
	pending  []rawHTTPInformationalResponse
	closed   bool
}

func bufferRawHTTPInformational(req *http.Request) *rawHTTPInformationalBuffer {
	trace := httptrace.ContextClientTrace(req.Context())
	buffer := &rawHTTPInformationalBuffer{trace: trace}
	if trace == nil || trace.Got1xxResponse == nil {
		return buffer
	}
	buffer.original = trace.Got1xxResponse
	trace.Got1xxResponse = func(status int, header textproto.MIMEHeader) error {
		buffer.mu.Lock()
		defer buffer.mu.Unlock()
		if !buffer.closed {
			buffer.pending = append(buffer.pending, rawHTTPInformationalResponse{status: status, header: textproto.MIMEHeader(http.Header(header).Clone())})
		}
		return nil
	}
	return buffer
}

func (b *rawHTTPInformationalBuffer) commit() {
	b.complete(true)
}

func (b *rawHTTPInformationalBuffer) discard() {
	b.complete(false)
}

func (b *rawHTTPInformationalBuffer) complete(commit bool) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	if b.trace != nil {
		b.trace.Got1xxResponse = b.original
	}
	pending := b.pending
	b.pending = nil
	b.mu.Unlock()
	if commit && b.original != nil {
		for _, response := range pending {
			_ = b.original(response.status, response.header)
		}
	}
}
