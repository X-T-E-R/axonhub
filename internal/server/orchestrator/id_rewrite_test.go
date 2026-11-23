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
	// 1. Same owner -> preserves previous_response_id untouched.
	bodyWithPrev := []byte(`{"model":"gpt-5","previous_response_id":"resp_123","input":"continue"}`)
	reqSame := &httpclient.Request{Body: append([]byte(nil), bodyWithPrev...)}
	err := enforceContinuationGuard(reqSame, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: bodyWithPrev}, &contexts.SessionAffinityDecision{
		StateOwnership: contexts.SessionStateOwnershipSame,
	})
	require.NoError(t, err)
	require.Equal(t, "resp_123", gjson.GetBytes(reqSame.Body, "previous_response_id").String())

	// 2. Different or Unknown owner WITHOUT full replayable history -> 409 continuation_owner_unavailable!
	for _, ownership := range []string{contexts.SessionStateOwnershipDifferent, contexts.SessionStateOwnershipUnknown} {
		reqNoHist := &httpclient.Request{Body: append([]byte(nil), bodyWithPrev...)}
		err := enforceContinuationGuard(reqNoHist, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: bodyWithPrev}, &contexts.SessionAffinityDecision{
			StateOwnership: ownership,
		})
		require.Error(t, err)
		var respErr *httpclient.Error
		require.ErrorAs(t, err, &respErr)
		require.Equal(t, http.StatusConflict, respErr.StatusCode)
		require.Contains(t, string(respErr.Body), "continuation_owner_unavailable")
		require.Equal(t, "resp_123", gjson.GetBytes(reqNoHist.Body, "previous_response_id").String())
	}

	// 3. Different owner WITH full replayable conversation history -> safely strips previous_response_id.
	fullHistBody := []byte(`{"model":"gpt-5","previous_response_id":"resp_123","input":[{"role":"user","content":"turn 1"},{"role":"assistant","content":"answer 1"},{"role":"user","content":"turn 2"}]}`)
	reqWithHist := &httpclient.Request{Body: append([]byte(nil), fullHistBody...)}
	err = enforceContinuationGuard(reqWithHist, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: fullHistBody}, &contexts.SessionAffinityDecision{
		StateOwnership: contexts.SessionStateOwnershipDifferent,
	})
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(reqWithHist.Body, "previous_response_id").Exists(), "full replayable history allows safe stripping of previous_response_id")

	// 4. Partial delta containing account-bound encrypted_content or item_reference MUST be rejected with 409!
	opaqueBody := []byte(`{"model":"gpt-5","previous_response_id":"resp_123","input":[{"role":"user","content":"t1"},{"role":"assistant","content":"a1","encrypted_content":"enc_opaque"},{"role":"user","content":"t2"}]}`)
	reqOpaque := &httpclient.Request{Body: append([]byte(nil), opaqueBody...)}
	err = enforceContinuationGuard(reqOpaque, &contexts.SessionAffinityRequest{PreviousResponseID: "resp_123", RawBody: opaqueBody}, &contexts.SessionAffinityDecision{
		StateOwnership: contexts.SessionStateOwnershipDifferent,
	})
	require.Error(t, err)
	var opaqueErr *httpclient.Error
	require.ErrorAs(t, err, &opaqueErr)
	require.Equal(t, http.StatusConflict, opaqueErr.StatusCode)

	// 5. Stream error events MUST NOT confirm session affinity success!
	require.True(t, isStreamErrorEvent(&httpclient.StreamEvent{Type: "error", Data: []byte(`{"type":"error","error":{"message":"rate limited"}}`)}))
	require.True(t, isStreamErrorEvent(&httpclient.StreamEvent{Type: "message", Data: []byte(`{"error":{"type":"overloaded_error"}}`)}))
	require.False(t, isStreamErrorEvent(&httpclient.StreamEvent{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_ok"}}`)}))

	// 6. X-Goog-Api-Key is protected and rejected by CompileChannelIDRewriteRules.
	_, err = biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "header:X-Goog-Api-Key",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32,
	}})
	require.Error(t, err)
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

func makeSizedJSONBody(targetBytes int) []byte {
	prefix := `{"model":"claude-3-7-sonnet","metadata":{"user_id":"user_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef_account__session_11111111-2222-3333-4444-555555555555"},"messages":[{"role":"user","content":"`
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

func BenchmarkIDRewrite_JSONPathMiss(b *testing.B) {
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

func BenchmarkIDRewrite_JSONScalar_Under64KiB(b *testing.B) {
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
		// Alternate key every iteration so derived != current and splice ALWAYS executes.
		req.Body = workBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}

func BenchmarkIDRewrite_JSONScalar_2MiB(b *testing.B) {
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

func BenchmarkIDRewrite_JSONScalar_2MiB_LengthChanging(b *testing.B) {
	plan, err := biz.CompileChannelIDRewriteRules([]objects.ChannelIDRewriteRule{{
		Target: "json:metadata.user_id",
		Op:     objects.ChannelIDRewriteOpDerive,
		When:   objects.ChannelIDRewriteWhenAlways,
		Format: objects.ChannelIDRewriteFormatHex32, // 32 bytes vs 123-byte original user_id
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
		// Restore original 123-byte metadata.user_id field header (200 bytes) before each run
		// so every iteration performs a real length-changing (123 -> 32) rewrite on a 2 MiB body.
		copy(workBody[:200], origBody[:200])
		req.Body = workBody
		ApplyCompiledIDRewritePlan(plan, secret, "1", 51, keys[i&1], "a1", false, nil, origBody, req)
	}
}
