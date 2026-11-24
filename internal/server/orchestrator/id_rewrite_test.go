package orchestrator

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestIDRewrite_All7Formats_RFC9562UUIDv8_And_RFC4122UUIDv5(t *testing.T) {
	secret := []byte("axon-test-hmac-secret-2026")
	origUserID := "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account__session_11111111-2222-3333-4444-555555555555"

	rules := []objects.ChannelIDRewriteRule{
		{Target: "header:X-Hex32", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatHex32, IfMissing: objects.ChannelIDRewriteIfMissingGenerate},
		{Target: "header:X-Hex64", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatHex64, IfMissing: objects.ChannelIDRewriteIfMissingGenerate},
		{Target: "header:X-PrefixHex8", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatPrefixHex8, Prefix: "p8_", IfMissing: objects.ChannelIDRewriteIfMissingGenerate},
		{Target: "header:X-PrefixHex16", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatPrefixHex16, Prefix: "p16_", IfMissing: objects.ChannelIDRewriteIfMissingGenerate},
		{Target: "header:X-UUIDv5", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatUUIDv5, IfMissing: objects.ChannelIDRewriteIfMissingGenerate},
		{Target: "header:X-UUIDv8", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatUUIDv8, IfMissing: objects.ChannelIDRewriteIfMissingGenerate},
		{Target: "json:metadata.user_id", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatAnthropicUserID},
		{Target: "json:metadata.fallback_user", Op: objects.ChannelIDRewriteOpDerive, When: objects.ChannelIDRewriteWhenAlways, Format: objects.ChannelIDRewriteFormatAnthropicUserID},
	}

	plan, err := biz.CompileChannelIDRewriteRules(rules)
	require.NoError(t, err)

	origBody := []byte(`{"model":"claude-3-7-sonnet","metadata":{"user_id":"` + origUserID + `","fallback_user":"plain-user-42"},"messages":[{"role":"user","content":"hi"}]}`)
	req := &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "https://api.kimi.com/coding/v1/messages",
		Headers: http.Header{},
		Body:    append([]byte(nil), origBody...),
	}

	ApplyCompiledIDRewritePlan(plan, secret, "proj-1", 51, "key-fp-A", "anchor-abc", false, http.Header{}, origBody, req)

	// 1. hex32, hex64, prefix_hex8, prefix_hex16 lengths and valid hex.
	h32 := req.Headers.Get("X-Hex32")
	h64 := req.Headers.Get("X-Hex64")
	p8 := req.Headers.Get("X-PrefixHex8")
	p16 := req.Headers.Get("X-PrefixHex16")
	require.Len(t, h32, 32)
	require.Len(t, h64, 64)
	require.True(t, strings.HasPrefix(p8, "p8_"))
	require.Len(t, strings.TrimPrefix(p8, "p8_"), 8)
	require.True(t, strings.HasPrefix(p16, "p16_"))
	require.Len(t, strings.TrimPrefix(p16, "p16_"), 16)
	_, err = hex.DecodeString(h64)
	require.NoError(t, err)

	// 2. True RFC 4122 UUIDv5 (SHA-1): version nibble == 5, RFC 4122 variant, deterministic.
	u5Str := req.Headers.Get("X-UUIDv5")
	u5, err := uuid.Parse(u5Str)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(5), u5.Version(), "uuid_v5 MUST have RFC 4122 version 5 (SHA-1)")
	require.Equal(t, uuid.RFC4122, u5.Variant())

	// 3. True RFC 9562 UUIDv8 (HMAC-SHA256): version nibble == 8, RFC 4122/9562 variant.
	u8Str := req.Headers.Get("X-UUIDv8")
	u8, err := uuid.Parse(u8Str)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(8), u8.Version(), "uuid_v8 MUST have RFC 9562 version 8")
	require.Equal(t, uuid.RFC4122, u8.Variant())
	require.NotEqual(t, u5Str, u8Str)

	// 4. anthropic_user_id preserves composite structure AND handles plain fallback cleanly.
	rewrittenComposite := gjson.GetBytes(req.Body, "metadata.user_id").String()
	require.True(t, strings.HasPrefix(rewrittenComposite, "user_"))
	require.Contains(t, rewrittenComposite, "_account__session_")
	require.NotEqual(t, origUserID, rewrittenComposite)
	parts := strings.Split(rewrittenComposite, "_account__session_")
	require.Len(t, parts, 2)
	require.Len(t, strings.TrimPrefix(parts[0], "user_"), 64)
	parsedSess, err := uuid.Parse(parts[1])
	require.NoError(t, err)
	require.Equal(t, uuid.Version(8), parsedSess.Version())

	rewrittenFallback := gjson.GetBytes(req.Body, "metadata.fallback_user").String()
	require.True(t, strings.HasPrefix(rewrittenFallback, "user_"))
	require.Len(t, strings.TrimPrefix(rewrittenFallback, "user_"), 64, "non-composite anthropic_user_id falls back to user_<64hex>")

	// 5. Retry immutability: retrying on key-fp-B using the original immutable ingress snapshot
	// produces the EXACT same result as running directly on key-fp-B (NO double hashing!).
	reqRetry := &httpclient.Request{
		Method:  http.MethodPost,
		Headers: http.Header{},
		Body:    append([]byte(nil), req.Body...),
	}
	ApplyCompiledIDRewritePlan(plan, secret, "proj-1", 51, "key-fp-B", "anchor-abc", true, http.Header{}, origBody, reqRetry)

	reqDirectB := &httpclient.Request{
		Method:  http.MethodPost,
		Headers: http.Header{},
		Body:    append([]byte(nil), origBody...),
	}
	ApplyCompiledIDRewritePlan(plan, secret, "proj-1", 51, "key-fp-B", "anchor-abc", true, http.Header{}, origBody, reqDirectB)

	require.Equal(t, gjson.GetBytes(reqDirectB.Body, "metadata.user_id").String(), gjson.GetBytes(reqRetry.Body, "metadata.user_id").String(), "retry MUST derive from immutable ingress snapshot, never double-hash attempt 1 output")
	require.NotEqual(t, rewrittenComposite, gjson.GetBytes(reqRetry.Body, "metadata.user_id").String(), "different keys must derive distinct identities")
}

func TestIDRewrite_ContinuationGuardStrictReplay(t *testing.T) {
	// 1. strict_replay + Same owner + outPrev == inPrev -> preserves previous_response_id untouched.
	bodyWithPrev := []byte(`{"model":"gpt-5","previous_response_id":"resp_123","input":"continue"}`)
	reqSame := &httpclient.Request{Body: append([]byte(nil), bodyWithPrev...)}
	err := enforceContinuationGuard(reqSame, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: bodyWithPrev}, &contexts.SessionAffinityDecision{
		StateOwnership: contexts.SessionStateOwnershipSame,
	}, objects.ChannelContinuationGuardStrictReplay)
	require.NoError(t, err)
	require.Equal(t, "resp_123", gjson.GetBytes(reqSame.Body, "previous_response_id").String())

	// 2. strict_replay + Same owner BUT static Body Override deleted or mutated previous_response_id -> 409!
	for _, mutated := range [][]byte{
		[]byte(`{"model":"gpt-5","input":"continue"}`),
		[]byte(`{"model":"gpt-5","previous_response_id":"resp_tampered","input":"continue"}`),
	} {
		reqMut := &httpclient.Request{Body: append([]byte(nil), mutated...)}
		err := enforceContinuationGuard(reqMut, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: bodyWithPrev}, &contexts.SessionAffinityDecision{
			StateOwnership: contexts.SessionStateOwnershipSame,
		}, objects.ChannelContinuationGuardStrictReplay)
		require.Error(t, err)
		var respErr *httpclient.Error
		require.ErrorAs(t, err, &respErr)
		require.Equal(t, http.StatusConflict, respErr.StatusCode)
		require.Contains(t, string(respErr.Body), "continuation_owner_unavailable")
	}

	// 3. strict_replay + Different or Unknown owner -> ALWAYS 409 continuation_owner_unavailable,
	// even if input happens to look like user -> assistant -> user (cannot distinguish sliding window from full history).
	fullLookingBody := []byte(`{"model":"gpt-5","previous_response_id":"resp_123","input":[{"role":"user","content":"turn 1"},{"role":"assistant","content":"answer 1"},{"role":"user","content":"turn 2"}]}`)
	for _, ownership := range []string{contexts.SessionStateOwnershipDifferent, contexts.SessionStateOwnershipUnknown} {
		for _, body := range [][]byte{bodyWithPrev, fullLookingBody} {
			reqCross := &httpclient.Request{Body: append([]byte(nil), body...)}
			err := enforceContinuationGuard(reqCross, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: body}, &contexts.SessionAffinityDecision{
				StateOwnership: ownership,
			}, objects.ChannelContinuationGuardStrictReplay)
			require.Error(t, err)
			var respErr *httpclient.Error
			require.ErrorAs(t, err, &respErr)
			require.Equal(t, http.StatusConflict, respErr.StatusCode)
			require.Contains(t, string(respErr.Body), "continuation_owner_unavailable")
			require.Equal(t, "resp_123", gjson.GetBytes(reqCross.Body, "previous_response_id").String(), "strict_replay must never silently strip previous_response_id")
		}
	}

	// 4. continuationGuard == off + Different/Unknown owner -> strips previous_response_id, item_reference, and encrypted_content.
	offBody := []byte(`{"model":"gpt-5","previous_response_id":"resp_123","input":[{"type":"item_reference","id":"rs_1"},{"role":"user","content":"t1"},{"role":"assistant","content":"a1","encrypted_content":"enc_opaque"},{"type":"item_reference","id":"rs_2"},{"role":"user","content":"t2"}]}`)
	reqOff := &httpclient.Request{Body: append([]byte(nil), offBody...)}
	err = enforceContinuationGuard(reqOff, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: offBody}, &contexts.SessionAffinityDecision{
		StateOwnership: contexts.SessionStateOwnershipDifferent,
	}, objects.ChannelContinuationGuardOff)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(reqOff.Body, "previous_response_id").Exists())
	require.Equal(t, int64(3), gjson.GetBytes(reqOff.Body, "input.#").Int(), "item_reference entries must be stripped under continuationGuard=off")
	require.False(t, gjson.GetBytes(reqOff.Body, "input.1.encrypted_content").Exists(), "encrypted_content must be stripped under continuationGuard=off")

	// 5. X-Goog-Api-Key is protected and rejected by CompileChannelIDRewriteRules.
	_, err = biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "header:X-Goog-Api-Key",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32,
	}})
	require.Error(t, err)
}

func TestIDRewrite_StreamSuccessEventStrictValidation(t *testing.T) {
	// Non-business / heartbeat / incomplete / error frames MUST return false.
	negativeCases := []*httpclient.StreamEvent{
		nil,
		{Type: "", Data: nil},
		{Type: "message", Data: []byte("   ")},
		{Type: "message", Data: []byte("[DONE]")},
		{Type: "message", Data: []byte(": keepalive")},
		{Type: "ping", Data: []byte(`{"type":"ping"}`)},
		{Type: "message", Data: []byte(`{"type":"ping"}`)},
		{Type: "message", Data: []byte(`{}`)},
		{Type: "message", Data: []byte(`{"choices":[]}`)},
		{Type: "message", Data: []byte(`{"candidates":[]}`)},
		{Type: "message", Data: []byte(`{"type":"message_start"`)}, // truncated JSON
		{Type: "error", Data: []byte(`{"type":"error","error":{"message":"rate limited"}}`)},
		{Type: "message", Data: []byte(`{"error":{"type":"overloaded_error"}}`)},
		{Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"id":"resp_1"}}`)},
		{Type: "response.incomplete", Data: []byte(`{"type":"response.incomplete","response":{"id":"resp_1"}}`)},
	}
	for i, ev := range negativeCases {
		require.False(t, isStreamSuccessEvent(ev), "negative case %d must NOT confirm CAS rebind", i)
	}

	// Explicit positive protocol business events MUST return true.
	positiveCases := []*httpclient.StreamEvent{
		{Type: "message_start", Data: []byte(`{"type":"message_start","message":{"id":"msg_1"}}`)},
		{Type: "content_block_start", Data: []byte(`{"type":"content_block_start","index":0}`)},
		{Type: "content_block_delta", Data: []byte(`{"type":"content_block_delta","delta":{"text":"hi"}}`)},
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_ok"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","item":{"id":"item_1"}}`)},
		{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"hello"}`)},
		{Type: "message", Data: []byte(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hi"}}]}`)},
		{Type: "message", Data: []byte(`{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)},
	}
	for i, ev := range positiveCases {
		require.True(t, isStreamSuccessEvent(ev), "positive case %d MUST confirm CAS rebind", i)
	}
}

func TestIDRewrite_MissingSecretDoesNotBlameUpstreamKeyOrFailurePolicy(t *testing.T) {
	t.Setenv("AXONHUB_ID_REWRITE_SECRET", "")
	t.Setenv("AXONHUB_ENCRYPTION_KEY", "")

	ch := &biz.Channel{
		Channel: &ent.Channel{
			ID:          77,
			Name:        "kimi-missing-secret",
			Type:        channel.TypeMoonshotCoding,
			Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-a", "sk-b"}},
			Settings: &objects.ChannelSettings{
				IDRewriteRules: []objects.ChannelIDRewriteRule{{
					Target: "json:metadata.user_id",
					Op:     objects.ChannelIDRewriteOpDerive,
					When:   objects.ChannelIDRewriteWhenAlways,
					Format: objects.ChannelIDRewriteFormatAnthropicUserID,
				}},
			},
		},
	}

	outbound := &PersistentOutboundTransformer{
		state: &PersistenceState{
			CurrentCandidate: &ChannelModelsCandidate{Channel: ch},
			Perf:             &biz.PerformanceRecord{ChannelID: ch.ID},
		},
	}
	mw := &sessionAffinityIDRewriteMiddleware{outbound: outbound}

	var finishedCalls int
	var gotFailed, gotCanceled bool
	ctx := contexts.WithChannelCredentialEgress(context.Background(), "sk-a", nil, func(_ int, failed, canceled bool) {
		finishedCalls++
		gotFailed = failed
		gotCanceled = canceled
	})
	body := []byte(`{"model":"kimi","metadata":{"user_id":"u1"}}`)
	contexts.WithSessionAffinityRequest(ctx, &contexts.SessionAffinityRequest{RawBody: body})

	_, err := mw.OnOutboundRawRequest(ctx, &httpclient.Request{Body: body})
	require.ErrorIs(t, err, biz.ErrIDRewriteSecretMissing())
	require.Equal(t, 1, finishedCalls)
	require.False(t, gotFailed, "local missing secret must NEVER mark upstream API key as failed")
	require.True(t, gotCanceled, "local missing secret must release credential attempt as canceled")

	// Also confirm performanceRecording.OnOutboundRawError never marks perf failed or triggers FailurePolicy.
	perfMw := &performanceRecording{outbound: outbound}
	perfMw.OnOutboundRawError(ctx, err)
	require.False(t, outbound.state.FailurePolicyRoutingChanged)
	require.False(t, outbound.credentialRetryPending)
	require.Zero(t, outbound.state.Perf.ResponseStatusCode)
}

func TestProviderQuotaSelector_SkipsPerKeyAuthorityChannels(t *testing.T) {
	ch := &biz.Channel{
		Channel: &ent.Channel{
			ID:          51,
			Name:        "kimi-ch51",
			Type:        channel.TypeMoonshotCoding,
			Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-1", "sk-2", "sk-3"}},
			Settings: &objects.ChannelSettings{
				BalanceProbe: &objects.ChannelBalanceProbe{
					Enabled: true,
					Preset:  objects.ChannelBalanceProbePresetKimiCode,
				},
			},
		},
	}
	base := &stubCandidateSelector{candidates: []*ChannelModelsCandidate{{Channel: ch}}}
	pq := &stubQuotaStatusReader{
		statuses: map[int]*biz.QuotaChannelStatus{
			51: {Status: providerquotastatus.StatusExhausted},
		},
	}
	selector := WithProviderQuotaSelector(base, pq, &stubQuotaEnforcementSettings{})
	out, err := selector.Select(context.Background(), &llm.Request{Model: "kimi-for-coding"})
	require.NoError(t, err)
	require.Len(t, out, 1, "per-key authority channel must ignore stale provider_quota_status row during routing")
	require.Equal(t, 51, out[0].Channel.ID)
}

type stubCandidateSelector struct {
	candidates []*ChannelModelsCandidate
}

func (s *stubCandidateSelector) Select(_ context.Context, _ *llm.Request) ([]*ChannelModelsCandidate, error) {
	return s.candidates, nil
}

type stubQuotaStatusReader struct {
	statuses map[int]*biz.QuotaChannelStatus
}

func (s *stubQuotaStatusReader) GetQuotaStatus(channelID int) *biz.QuotaChannelStatus {
	return s.statuses[channelID]
}

type stubQuotaEnforcementSettings struct{}

func (s *stubQuotaEnforcementSettings) QuotaEnforcementSettingsOrDefault(_ context.Context) *biz.QuotaEnforcementSettings {
	return &biz.QuotaEnforcementSettings{
		Enabled: true,
		Mode:    biz.QuotaEnforcementModeExhaustedOnly,
	}
}

// --- Benchmarks (Reality Budget verification) ---

func makeSizedJSONBodyWithUserID(targetBytes int, userID string) []byte {
	prefix := `{"model":"claude-3-7-sonnet","metadata":{"user_id":"` + userID + `"},"messages":[{"role":"user","content":"`
	suffix := `"}]}`
	padLen := targetBytes - len(prefix) - len(suffix)
	if padLen < 16 {
		padLen = 16
	}
	var buf bytes.Buffer
	buf.Grow(len(prefix) + padLen + len(suffix))
	buf.WriteString(prefix)
	buf.Write(bytes.Repeat([]byte("a"), padLen))
	buf.WriteString(suffix)
	return buf.Bytes()
}

func makeSizedJSONBody(targetBytes int) []byte {
	return makeSizedJSONBodyWithUserID(targetBytes, "user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account__session_11111111-2222-3333-4444-555555555555")
}

func BenchmarkIDRewrite_NoRules(b *testing.B) {
	body := makeSizedJSONBody(32 * 1024)
	req := &httpclient.Request{Body: body, Headers: http.Header{}}
	secret := []byte("bench-secret")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ApplyCompiledIDRewritePlan(nil, secret, "1", 51, "k1", "a1", false, nil, body, req)
	}
	if &req.Body[0] != &body[0] {
		b.Fatal("expected byte-identical slice when no rules")
	}
}

func BenchmarkIDRewrite_HeaderOnly(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target:    "header:X-Client-Session",
		Op:        objects.ChannelIDRewriteOpDerive,
		When:      objects.ChannelIDRewriteWhenAlways,
		Format:    objects.ChannelIDRewriteFormatUUIDv8,
		IfMissing: objects.ChannelIDRewriteIfMissingGenerate,
	}})
	if err != nil {
		b.Fatal(err)
	}
	body := makeSizedJSONBody(64 * 1024)
	origHeaders := http.Header{"X-Client-Session": []string{"sess-1"}}
	req := &httpclient.Request{Body: body, Headers: make(http.Header, 2)}
	secret := []byte("bench-secret")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, "k1", "a1", false, origHeaders, body, req)
	}
	if &req.Body[0] != &body[0] {
		b.Fatal("header-only rule must NEVER touch or allocate request body")
	}
}

func BenchmarkIDRewrite_JSONPathMiss_64KiB(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target:    "json:metadata.nonexistent_field",
		Op:        objects.ChannelIDRewriteOpDerive,
		When:      objects.ChannelIDRewriteWhenAlways,
		Format:    objects.ChannelIDRewriteFormatHex32,
		IfMissing: objects.ChannelIDRewriteIfMissingSkip,
	}})
	if err != nil {
		b.Fatal(err)
	}
	body := makeSizedJSONBody(64 * 1024)
	req := &httpclient.Request{Body: body, Headers: http.Header{}}
	secret := []byte("bench-secret")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, "k1", "a1", false, nil, body, req)
	}
	if &req.Body[0] != &body[0] {
		b.Fatal("JSON path miss must NEVER reallocate request body")
	}
}

func BenchmarkIDRewrite_JSONScalar_32KiB_InPlace(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatAnthropicUserID,
	}})
	if err != nil {
		b.Fatal(err)
	}
	origBody := makeSizedJSONBody(32 * 1024)
	workBody := append([]byte(nil), origBody...)
	req := &httpclient.Request{Body: workBody, Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = workBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}

func BenchmarkIDRewrite_JSONScalar_32KiB_SharedReadOnlyIngress(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatAnthropicUserID,
	}})
	if err != nil {
		b.Fatal(err)
	}
	// Pass-through mode: request.Body aliases immutable ingress origBody.
	origBody := makeSizedJSONBody(32 * 1024)
	origSnapshot := append([]byte(nil), origBody...)
	req := &httpclient.Request{Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = origBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
	b.StopTimer()
	if !bytes.Equal(origBody, origSnapshot) {
		b.Fatal("shared read-only ingress body must never be mutated in place")
	}
}

func BenchmarkIDRewrite_JSONScalar_32KiB_Growing(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex64, // 2-byte "u1" -> 64-byte hex64 (growing splice)
	}})
	if err != nil {
		b.Fatal(err)
	}
	origBody := makeSizedJSONBodyWithUserID(32*1024, "u1")
	workBody := make([]byte, len(origBody), len(origBody)+128)
	copy(workBody, origBody)
	req := &httpclient.Request{Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = append(workBody[:0], origBody...)
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}

func BenchmarkIDRewrite_JSONScalar_2MiB_InPlace(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatAnthropicUserID,
	}})
	if err != nil {
		b.Fatal(err)
	}
	origBody := makeSizedJSONBody(2 * 1024 * 1024)
	workBody := append([]byte(nil), origBody...)
	req := &httpclient.Request{Body: workBody, Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = workBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}

func BenchmarkIDRewrite_JSONScalar_2MiB_SharedReadOnlyIngress(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatAnthropicUserID,
	}})
	if err != nil {
		b.Fatal(err)
	}
	origBody := makeSizedJSONBody(2 * 1024 * 1024)
	req := &httpclient.Request{Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = origBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}

func BenchmarkIDRewrite_JSONScalar_2MiB_Growing(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex64, // 2-byte "u1" -> 64-byte hex64 (growing splice)
	}})
	if err != nil {
		b.Fatal(err)
	}
	origBody := makeSizedJSONBodyWithUserID(2*1024*1024, "u1")
	workBody := make([]byte, len(origBody), len(origBody)+128)
	copy(workBody, origBody)
	req := &httpclient.Request{Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Body = append(workBody[:0], origBody...)
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}

func BenchmarkIDRewrite_JSONScalar_2MiB_Shrinking(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32, // 123-byte original user_id -> 32-byte hex32
	}})
	if err != nil {
		b.Fatal(err)
	}
	origBody := makeSizedJSONBody(2 * 1024 * 1024)
	workBody := append([]byte(nil), origBody...)
	req := &httpclient.Request{Headers: http.Header{}}
	secret := []byte("bench-secret")
	keys := [2]string{"key-A", "key-B"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		copy(workBody[:200], origBody[:200])
		req.Body = workBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}
