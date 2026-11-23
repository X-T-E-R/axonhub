package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	rawHTTPRetryBodyLimit    = 2 << 20
	rawHTTPErrorInspectLimit = 64 << 10
	rawHTTPMaxAttempts       = 3
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
	rawPath        string
	initialKey     string
	initialAttempt *biz.RawHTTPCredentialAttempt
	maxAttempts    int
	disableRetries bool

	mu                    sync.Mutex
	transports            []*http.Transport
	activeAttempt         *biz.RawHTTPCredentialAttempt
	observedFailureStatus atomic.Int32
}

func (t *rawHTTPRetryTransport) setActiveAttempt(a *biz.RawHTTPCredentialAttempt) {
	t.mu.Lock()
	t.activeAttempt = a
	t.mu.Unlock()
}

// FinishActiveAttempt releases the most recently reserved credential attempt if
// ReverseProxy exited without closing the response body (e.g. rejected upgrade).
func (t *rawHTTPRetryTransport) FinishActiveAttempt() {
	t.mu.Lock()
	active := t.activeAttempt
	t.activeAttempt = nil
	t.mu.Unlock()
	active.Finish(0, biz.CredentialAttemptCanceled)
}

// ObservedFailureStatus returns the last authoritative upstream failure HTTP
// status observed on this transport (or 0 if none was observed).
func (t *rawHTTPRetryTransport) ObservedFailureStatus() int {
	return int(t.observedFailureStatus.Load())
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

	rawPath := t.resolveRawPath(req)
	attemptHandle := t.initialAttemptHandle(ctx)
	t.setActiveAttempt(attemptHandle)
	triedKeys := make(map[string]struct{}, maxAttempts)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			attemptHandle.Finish(0, biz.CredentialAttemptCanceled)
			return nil, err
		}
		triedKeys[attemptHandle.Key] = struct{}{}

		attemptStarted := time.Now()
		resp, informational, closeTransport, roundTripErr := t.executeAttempt(ctx, req, body, maxAttempts > 1, attemptHandle)
		if roundTripErr != nil && resp == nil {
			canceled := ctx.Err() != nil || errors.Is(roundTripErr, context.Canceled) || errors.Is(roundTripErr, context.DeadlineExceeded)
			if !canceled && canReplay && attempt < maxAttempts {
				// Release the prior attempt's key and proxy lease before reserving
				// an alternate so shared proxyLimit=1 pools never self-deadlock.
				attemptHandle.Finish(0, biz.CredentialAttemptFailure)
				t.applyFailurePolicy(ctx, attemptHandle, http.StatusBadGateway, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
				nextAttempt := t.channels.ReserveRawHTTPCredential(ctx, t.channel, triedKeys, false)
				if nextAttempt != nil {
					informational.discard()
					closeTransport()
					attemptHandle = nextAttempt
					t.setActiveAttempt(attemptHandle)
					continue
				}
				informational.discard()
				return nil, roundTripErr
			}

			informational.discard()
			if canceled {
				attemptHandle.Finish(0, biz.CredentialAttemptCanceled)
			} else {
				attemptHandle.Finish(0, biz.CredentialAttemptFailure)
				t.applyFailurePolicy(ctx, attemptHandle, http.StatusBadGateway, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
			}
			return nil, roundTripErr
		}
		if resp == nil {
			informational.discard()
			attemptHandle.Finish(http.StatusBadGateway, biz.CredentialAttemptFailure)
			t.applyFailurePolicy(ctx, attemptHandle, http.StatusBadGateway, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
			return nil, errors.New("raw HTTP transport returned no response")
		}

		// Dedicated 403 branch: classified structured key failure or optional
		// 1-alternate differential recovery on safe operations. Bounded to at
		// most one alternate attempt after a 403.
		if resp.StatusCode == http.StatusForbidden {
			return t.handleForbiddenResponse(
				ctx, req, body, rawPath,
				canReplay && attempt < maxAttempts,
				attemptHandle, attemptStarted,
				resp, informational, closeTransport, triedKeys,
			)
		}

		if retryableRawHTTPStatus(resp.StatusCode) && !t.disableRetries && canReplay && ctx.Err() == nil && attempt < maxAttempts {
			t.observedFailureStatus.Store(int32(resp.StatusCode))
			// Release the prior attempt's key and proxy lease before reserving
			// an alternate strictly (allowSoftFallback=false).
			attemptHandle.Finish(resp.StatusCode, biz.CredentialAttemptFailure)
			t.applyFailurePolicy(ctx, attemptHandle, resp.StatusCode, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
			nextAttempt := t.channels.ReserveRawHTTPCredential(ctx, t.channel, triedKeys, false)
			if nextAttempt != nil {
				informational.discard()
				if resp.Body != nil {
					_ = resp.Body.Close()
				}
				closeTransport()
				attemptHandle = nextAttempt
				t.setActiveAttempt(attemptHandle)
				continue
			}
			// Selection exhausted: preserve the latest upstream response unchanged.
			informational.discard()
			return resp, nil
		}

		return t.finalizeResponse(ctx, req, rawPath, attemptHandle, attemptStarted, resp, informational, false)
	}

	return nil, errors.New("raw HTTP retry budget exhausted")
}

func (t *rawHTTPRetryTransport) handleForbiddenResponse(
	ctx context.Context,
	req *http.Request,
	body []byte,
	rawPath string,
	hasRetryBudget bool,
	attemptA *biz.RawHTTPCredentialAttempt,
	startedA time.Time,
	respA *http.Response,
	infoA *rawHTTPInformationalBuffer,
	closeTransportA func(),
	triedKeys map[string]struct{},
) (*http.Response, error) {
	hasPolicy := t.channels.HasRawHTTP403KeyPolicy(ctx, t.channel)
	opaqueRecovery := t.opaque403RecoveryAllowed(req.Method, rawPath)
	if !hasPolicy && !opaqueRecovery {
		// Unconfigured Raw HTTP channel: 1-call pass-through with neutral lease release.
		return t.finalizeResponse(ctx, req, rawPath, attemptA, startedA, respA, infoA, true)
	}

	var errFields biz.StructuredJSONError
	var classification biz.RawHTTPKeyFailureClassification

	peeked, oversized, peekErr, restoredBody := peekRawHTTPErrorBody(respA)
	respA.Body = restoredBody
	if peekErr != nil {
		// Truncated/broken 403 body: never classify as 403 or replay; preserve
		// error so downstream read reports 502 transport failure on attempt A.
		return t.finalizeResponse(ctx, req, rawPath, attemptA, startedA, respA, infoA, true)
	}
	if hasPolicy && !oversized {
		errFields = biz.ExtractStructuredJSONError(peeked, respA.Header.Get("Content-Encoding"))
		if errFields.HasIdentifier() {
			classification = t.channels.ClassifyRawHTTPKeyFailure(ctx, t.channel, attemptA.Key, req.Method, rawPath, respA.StatusCode, errFields)
		}
	}

	if classification.Classified {
		t.observedFailureStatus.Store(int32(respA.StatusCode))
		// Classified key-level 403: attribute key-only failure to A even if
		// replay is disabled, context is canceled, or no alternate is eligible.
		attemptA.Finish(respA.StatusCode, biz.CredentialAttemptFailure)
		t.applyFailurePolicy(ctx, attemptA, respA.StatusCode, req.Method, rawPath, errFields, startedA, true, true)

		if t.disableRetries || !hasRetryBudget || ctx.Err() != nil || !classification.AllowReplay {
			infoA.discard()
			return respA, nil
		}

		altAttempt := t.channels.ReserveRawHTTPCredential(ctx, t.channel, triedKeys, false)
		if altAttempt == nil {
			infoA.discard()
			return respA, nil
		}
		triedKeys[altAttempt.Key] = struct{}{}
		t.setActiveAttempt(altAttempt)

		infoA.discard()
		if respA.Body != nil {
			_ = respA.Body.Close()
		}
		closeTransportA()

		altStarted := time.Now()
		respB, infoB, _, errB := t.executeAttempt(ctx, req, body, true, altAttempt)
		if errB != nil || respB == nil {
			infoB.discard()
			altCanceled := ctx.Err() != nil || errors.Is(errB, context.Canceled) || errors.Is(errB, context.DeadlineExceeded)
			if altCanceled {
				altAttempt.Finish(0, biz.CredentialAttemptCanceled)
			} else {
				altAttempt.Finish(0, biz.CredentialAttemptFailure)
				t.applyFailurePolicy(ctx, altAttempt, http.StatusBadGateway, req.Method, rawPath, biz.StructuredJSONError{}, altStarted, false, false)
			}
			if errB != nil {
				return nil, errB
			}
			return nil, errors.New("raw HTTP transport returned no response")
		}
		if respB.StatusCode == http.StatusForbidden {
			return t.finalizeTerminalForbidden(ctx, req, rawPath, hasPolicy, altAttempt, altStarted, respB, infoB)
		}
		return t.finalizeResponse(ctx, req, rawPath, altAttempt, altStarted, respB, infoB, false)
	}

	// Opaque 403 (unmatched by any Expr key failure profile).
	if !opaqueRecovery || t.disableRetries || !hasRetryBudget || ctx.Err() != nil {
		return t.finalizeResponse(ctx, req, rawPath, attemptA, startedA, respA, infoA, true)
	}

	// Release A's key and proxy lease neutrally before reserving strict alternate B.
	attemptA.Finish(respA.StatusCode, biz.CredentialAttemptNeutral)
	altAttempt := t.channels.ReserveRawHTTPCredential(ctx, t.channel, triedKeys, false)
	if altAttempt == nil {
		return t.finalizeResponse(ctx, req, rawPath, attemptA, startedA, respA, infoA, true)
	}
	triedKeys[altAttempt.Key] = struct{}{}
	t.setActiveAttempt(altAttempt)

	// Discard A's informational buffer BEFORE executing B so B's Got1xxResponse
	// wraps the original downstream trace callback rather than A's discarded buffer.
	infoA.discard()

	altStarted := time.Now()
	respB, infoB, closeTransportB, errB := t.executeAttempt(ctx, req, body, true, altAttempt)
	if errB == nil && respB != nil && respB.StatusCode >= 200 && respB.StatusCode < 300 {
		// Alternate B returned 2xx: close A and forward B with zero penalty on A.
		if respA.Body != nil {
			_ = respA.Body.Close()
		}
		closeTransportA()
		return t.finalizeResponse(ctx, req, rawPath, altAttempt, altStarted, respB, infoB, false)
	}

	// B returned any non-2xx (403, 429, 503, 3xx, 304, 101, etc.) or a
	// transport error: handle B on its own evidence, close B, and return A's
	// original status, headers, and body unchanged with no 3rd call.
	infoB.discard()
	t.settleDiscardedAlternate(ctx, req, rawPath, hasPolicy, altAttempt, altStarted, respB, errB)
	if respB != nil && respB.Body != nil {
		_ = respB.Body.Close()
	}
	closeTransportB()
	t.setActiveAttempt(attemptA)

	return t.finalizeResponse(ctx, req, rawPath, attemptA, startedA, respA, infoA, true)
}

func (t *rawHTTPRetryTransport) finalizeTerminalForbidden(
	ctx context.Context,
	req *http.Request,
	rawPath string,
	hasPolicy bool,
	attempt *biz.RawHTTPCredentialAttempt,
	started time.Time,
	resp *http.Response,
	info *rawHTTPInformationalBuffer,
) (*http.Response, error) {
	if hasPolicy {
		peeked, oversized, peekErr, restored := peekRawHTTPErrorBody(resp)
		resp.Body = restored
		if peekErr == nil && !oversized {
			errFields := biz.ExtractStructuredJSONError(peeked, resp.Header.Get("Content-Encoding"))
			if errFields.HasIdentifier() {
				classification := t.channels.ClassifyRawHTTPKeyFailure(ctx, t.channel, attempt.Key, req.Method, rawPath, resp.StatusCode, errFields)
				if classification.Classified {
					t.observedFailureStatus.Store(int32(resp.StatusCode))
					info.discard()
					attempt.Finish(resp.StatusCode, biz.CredentialAttemptFailure)
					t.applyFailurePolicy(ctx, attempt, resp.StatusCode, req.Method, rawPath, errFields, started, true, true)
					return resp, nil
				}
			}
		}
	}
	return t.finalizeResponse(ctx, req, rawPath, attempt, started, resp, info, true)
}

func (t *rawHTTPRetryTransport) settleDiscardedAlternate(
	ctx context.Context,
	req *http.Request,
	rawPath string,
	hasPolicy bool,
	attempt *biz.RawHTTPCredentialAttempt,
	started time.Time,
	resp *http.Response,
	roundTripErr error,
) {
	if roundTripErr != nil || resp == nil {
		canceled := ctx.Err() != nil || errors.Is(roundTripErr, context.Canceled) || errors.Is(roundTripErr, context.DeadlineExceeded)
		if canceled {
			attempt.Finish(0, biz.CredentialAttemptCanceled)
			return
		}
		attempt.Finish(0, biz.CredentialAttemptFailure)
		t.applyFailurePolicy(ctx, attempt, http.StatusBadGateway, req.Method, rawPath, biz.StructuredJSONError{}, started, false, false)
		return
	}
	if resp.StatusCode == http.StatusForbidden {
		if hasPolicy {
			peeked, oversized, peekErr, restored := peekRawHTTPErrorBody(resp)
			resp.Body = restored
			if peekErr == nil && !oversized {
				errFields := biz.ExtractStructuredJSONError(peeked, resp.Header.Get("Content-Encoding"))
				if errFields.HasIdentifier() {
					classification := t.channels.ClassifyRawHTTPKeyFailure(ctx, t.channel, attempt.Key, req.Method, rawPath, resp.StatusCode, errFields)
					if classification.Classified {
						attempt.Finish(resp.StatusCode, biz.CredentialAttemptFailure)
						t.applyFailurePolicy(ctx, attempt, resp.StatusCode, req.Method, rawPath, errFields, started, true, true)
						return
					}
				}
			}
		}
		attempt.Finish(resp.StatusCode, biz.CredentialAttemptNeutral)
		return
	}
	if rawHTTPFailureStatus(resp.StatusCode) {
		attempt.Finish(resp.StatusCode, biz.CredentialAttemptFailure)
		t.applyFailurePolicy(ctx, attempt, resp.StatusCode, req.Method, rawPath, biz.StructuredJSONError{}, started, false, false)
		return
	}
	attempt.Finish(resp.StatusCode, biz.CredentialAttemptNeutral)
}

func (t *rawHTTPRetryTransport) finalizeResponse(
	ctx context.Context,
	req *http.Request,
	rawPath string,
	attemptHandle *biz.RawHTTPCredentialAttempt,
	attemptStarted time.Time,
	resp *http.Response,
	informational *rawHTTPInformationalBuffer,
	neutralOnComplete bool,
) (*http.Response, error) {
	// Raw HTTP strips Upgrade/Connection hop-by-hop headers, so a 101 response
	// cannot be upgraded by ReverseProxy (which would reject it without closing
	// a wrapped body). Close and settle the lease inside the transport.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		informational.discard()
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		attemptHandle.Finish(http.StatusSwitchingProtocols, biz.CredentialAttemptNeutral)
		return nil, errors.New("raw HTTP upstream returned unsupported 101 switching protocols")
	}

	if resp.StatusCode >= 400 || ctx.Err() != nil {
		informational.discard()
	} else {
		informational.commit()
	}

	authoritativeFailure := rawHTTPFailureStatus(resp.StatusCode)
	if authoritativeFailure {
		t.observedFailureStatus.Store(int32(resp.StatusCode))
	}
	completeOutcome := biz.CredentialAttemptSuccess
	if neutralOnComplete {
		completeOutcome = biz.CredentialAttemptNeutral
	}

	if resp.Body == nil {
		if authoritativeFailure {
			attemptHandle.Finish(resp.StatusCode, biz.CredentialAttemptFailure)
			t.applyFailurePolicy(ctx, attemptHandle, resp.StatusCode, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
		} else if ctx.Err() != nil {
			attemptHandle.Finish(resp.StatusCode, biz.CredentialAttemptCanceled)
		} else {
			attemptHandle.Finish(resp.StatusCode, completeOutcome)
		}
		resp.Body = http.NoBody
		return resp, nil
	}

	resp.Body = &rawHTTPAttemptResponseBody{
		ReadCloser: resp.Body,
		finish: func(readErr error) {
			if authoritativeFailure {
				// Authoritative upstream failure headers (401/402/429/432/5xx)
				// were already observed; downstream disconnect during body read
				// or close must not erase failure attribution.
				attemptHandle.Finish(resp.StatusCode, biz.CredentialAttemptFailure)
				t.applyFailurePolicy(ctx, attemptHandle, resp.StatusCode, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
				return
			}
			if readErr != io.EOF && ctx.Err() != nil {
				attemptHandle.Finish(resp.StatusCode, biz.CredentialAttemptCanceled)
				return
			}
			if readErr != nil && readErr != io.EOF {
				attemptHandle.Finish(http.StatusBadGateway, biz.CredentialAttemptFailure)
				t.applyFailurePolicy(ctx, attemptHandle, http.StatusBadGateway, req.Method, rawPath, biz.StructuredJSONError{}, attemptStarted, false, false)
				return
			}
			attemptHandle.Finish(resp.StatusCode, completeOutcome)
		},
	}
	return resp, nil
}

func (t *rawHTTPRetryTransport) executeAttempt(
	ctx context.Context,
	req *http.Request,
	body []byte,
	cloneReq bool,
	attempt *biz.RawHTTPCredentialAttempt,
) (*http.Response, *rawHTTPInformationalBuffer, func(), error) {
	attemptReq := req
	if cloneReq {
		attemptReq = req.Clone(ctx)
		attemptReq.Header = req.Header.Clone()
		if req.Body != nil && req.Body != http.NoBody {
			attemptReq.Body = io.NopCloser(bytes.NewReader(body))
		}
		attemptReq.GetBody = nil
	}
	setRawHTTPAuth(attemptReq.Header, t.authType, t.authHeader, attempt.Key)

	transport, isCloned, err := t.transportForAttempt(attempt)
	if err != nil {
		return nil, &rawHTTPInformationalBuffer{}, func() {}, err
	}
	closeTransport := func() {
		if isCloned {
			transport.CloseIdleConnections()
		}
	}

	informational := bufferRawHTTPInformational(attemptReq)
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
	return resp, informational, closeTransport, roundTripErr
}

func (t *rawHTTPRetryTransport) initialAttemptHandle(ctx context.Context) *biz.RawHTTPCredentialAttempt {
	if t.initialAttempt != nil {
		return t.initialAttempt
	}
	ctxKey, client, finish := contexts.TakeChannelCredentialAttempt(ctx)
	key := t.initialKey
	if ctxKey != "" {
		key = ctxKey
	}
	return biz.NewLegacyRawHTTPCredentialAttempt(t.channel.ID, t.channel.CredentialOwnerID(), key, client, finish)
}

func (t *rawHTTPRetryTransport) resolveRawPath(req *http.Request) string {
	if t.rawPath != "" {
		return biz.NormalizeRawHTTPPath(t.rawPath)
	}
	rel := req.URL.Path
	if t.channel != nil && t.channel.BaseURL != "" {
		if base, err := url.Parse(t.channel.BaseURL); err == nil {
			prefix := strings.TrimRight(base.Path, "/")
			if prefix != "" && strings.HasPrefix(rel, prefix) {
				rel = strings.TrimPrefix(rel, prefix)
			}
		}
	}
	return biz.NormalizeRawHTTPPath(rel)
}

func (t *rawHTTPRetryTransport) opaque403RecoveryAllowed(method, rawPath string) bool {
	if t.channel == nil || t.channel.Settings == nil || t.channel.Settings.RawHTTP == nil {
		return false
	}
	cfg := t.channel.Settings.RawHTTP
	if cfg.Opaque403Recovery == nil || !*cfg.Opaque403Recovery {
		return false
	}
	return biz.IsSafeRawHTTPReplayOperation(method, rawPath, cfg)
}

func (t *rawHTTPRetryTransport) transportForAttempt(attempt *biz.RawHTTPCredentialAttempt) (*http.Transport, bool, error) {
	if attempt != nil && attempt.RawTransport != nil {
		return attempt.RawTransport, false, nil
	}
	client := t.defaultClient
	if attempt != nil && attempt.EgressClient != nil {
		client = attempt.EgressClient
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
	t.FinishActiveAttempt()
	t.mu.Lock()
	transports := slices.Clone(t.transports)
	t.transports = nil
	t.mu.Unlock()
	for _, transport := range transports {
		transport.CloseIdleConnections()
	}
}

func (t *rawHTTPRetryTransport) applyFailurePolicy(
	ctx context.Context,
	attempt *biz.RawHTTPCredentialAttempt,
	status int,
	method string,
	rawPath string,
	errFields biz.StructuredJSONError,
	started time.Time,
	keyTargetOnly bool,
	requireExprMatch bool,
) {
	policyStatus := status
	if policyStatus == 0 {
		policyStatus = http.StatusBadGateway
	}
	perf := &biz.PerformanceRecord{
		ChannelID:          attempt.ChannelID,
		CredentialOwnerID:  attempt.CredentialOwnerID,
		APIKey:             attempt.Key,
		StartTime:          started,
		EndTime:            time.Now(),
		ResponseStatusCode: policyStatus,
		RequestCompleted:   true,
		RequestMethod:      strings.ToUpper(strings.TrimSpace(method)),
		RequestPath:        biz.NormalizeRawHTTPPath(rawPath),
		ErrorTag:           errFields.Tag,
		ErrorCode:          errFields.Code,
		ErrorType:          errFields.Type,
		KeyTargetOnly:      keyTargetOnly,
		RequireExprMatch:   requireExprMatch,
		HasRecoveryGen:     attempt.HasRecoveryGen,
		RecoveryGen:        attempt.RecoveryGen,
	}
	t.channels.ApplyRequestFailurePolicy(ctx, perf)
}

func peekRawHTTPErrorBody(resp *http.Response) (peeked []byte, oversized bool, readErr error, restored io.ReadCloser) {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return nil, false, nil, http.NoBody
	}
	original := resp.Body
	if resp.ContentLength > rawHTTPErrorInspectLimit {
		return nil, true, nil, original
	}

	buf, err := io.ReadAll(io.LimitReader(original, rawHTTPErrorInspectLimit+1))
	if err != nil {
		return buf, false, err, &rawHTTPRestoredBody{
			Reader: io.MultiReader(bytes.NewReader(buf), rawHTTPBodyError{err: err}),
			closer: original,
		}
	}
	if len(buf) > rawHTTPErrorInspectLimit {
		return nil, true, nil, &rawHTTPRestoredBody{
			Reader: io.MultiReader(bytes.NewReader(buf), original),
			closer: original,
		}
	}
	// Entire bounded body was read cleanly; close upstream reader now and serve
	// exact bytes from memory without holding the upstream connection open.
	_ = original.Close()
	return buf, false, nil, io.NopCloser(bytes.NewReader(buf))
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

// rawHTTPFailureStatus identifies upstream statuses that indicate credential,
// quota, rate-limit, or upstream service health failures. Other 4xx responses
// (such as 400, 403, 404, 422) on opaque Raw HTTP endpoints are normal
// resource/application outcomes and must not penalize or disable upstream keys.
func rawHTTPFailureStatus(status int) bool {
	if status >= http.StatusInternalServerError {
		return true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusTooManyRequests, 432:
		return true
	default:
		return false
	}
}

func retryableRawHTTPStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusTooManyRequests, 432,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
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
