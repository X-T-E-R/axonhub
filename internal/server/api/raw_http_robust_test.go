package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
)

type rawHTTPFullFixture struct {
	db        *ent.Client
	channels  *biz.ChannelService
	channelID int
	server    *httptest.Server
}

func newRawHTTPFullFixture(
	t *testing.T,
	upstream *httptest.Server,
	keys []string,
	mutateSettings func(*objects.ChannelSettings),
) *rawHTTPFullFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := enttest.NewEntClient(t, "sqlite3", "file:raw-http-robust-"+uuid.NewString()+"?mode=memory&_fk=1")
	channels := biz.NewChannelServiceForTest(db)
	t.Cleanup(func() {
		channels.Stop()
		_ = db.Close()
	})

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))
	settings := &objects.ChannelSettings{
		KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
		Proxy:        &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
		RawHTTP:      &objects.ChannelRawHTTP{Enabled: true, Resource: "exa-search", AuthType: "header", AuthHeader: "x-api-key"},
	}
	if mutateSettings != nil {
		mutateSettings(settings)
	}
	row := db.Channel.Create().
		SetName("Exa Raw Fixture").
		SetType(channel.TypeOpenai).
		SetStatus(channel.StatusEnabled).
		SetBaseURL(upstream.URL).
		SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
		SetSupportedModels([]string{"exa-search"}).
		SetDefaultTestModel("exa-search").
		SetSettings(settings).
		SaveX(ctx)

	handler := NewRawHTTPHandlers(RawHTTPHandlersParams{ChannelService: channels, Ent: db})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		reqCtx := ent.NewContext(authz.WithTestBypass(c.Request.Context()), db)
		reqCtx = contexts.WithAPIKey(reqCtx, &ent.APIKey{
			ID:        1,
			ProjectID: 1,
			Scopes:    []string{string(scopes.ScopeWriteRequests)},
		})
		c.Request = c.Request.WithContext(reqCtx)
	})
	router.Any("/raw/:channelID/*path", handler.Proxy)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	return &rawHTTPFullFixture{
		db:        db,
		channels:  channels,
		channelID: row.ID,
		server:    server,
	}
}

func exaTeamBlockedFailurePolicy(allowReplay *bool) *objects.ChannelFailurePolicy {
	return &objects.ChannelFailurePolicy{
		Mode: objects.ChannelFailurePolicyModeMerge,
		KeyProfiles: []objects.FailurePolicyProfile{{
			ID:                "exa-team-blocked",
			Name:              "Exa TEAM_BLOCKED",
			AllowReplay:       allowReplay,
			Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
			ConditionCombiner: objects.FailurePolicyConditionCombinerAnd,
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				StatusCodes: []int{http.StatusForbidden},
				Expr:        `status == 403 && method == "POST" && path == "/search" && errorTag == "TEAM_BLOCKED"`,
			},
			Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
		}},
		ChannelProfiles: []objects.FailurePolicyProfile{{
			ID:                "channel-403-sentinel",
			Name:              "Channel 403 Sentinel",
			Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
			ConditionCombiner: objects.FailurePolicyConditionCombinerAnd,
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				StatusCodes: []int{http.StatusForbidden},
			},
			Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableChannel}},
		}},
	}
}

func TestRawHTTPClassified403ExaTeamBlocked(t *testing.T) {
	t.Run("TEAM_BLOCKED on POST /search disables Key A only and recovers 200 on Key B without leaking 103", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		var seenKeys []string

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			key := r.Header.Get("x-api-key")
			mu.Lock()
			seenKeys = append(seenKeys, key)
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.Header().Set("Link", "</early>; rel=preload")
				w.WriteHeader(http.StatusEarlyHints)
				w.Header().Del("Link")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Your team has been blocked.","tag":"TEAM_BLOCKED"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"results":["ok"]}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"exa-key-a", "exa-key-b", "exa-key-c"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))
		// Global legacy 403 auto-disable must not hijack or increment channel 403 count.
		require.NoError(t, f.channels.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
			Enabled: true,
			AutoDisableChannel: biz.AutoDisableChannel{
				Enabled:  true,
				Statuses: []biz.AutoDisableChannelStatus{{Status: http.StatusForbidden, Times: 1}},
			},
		}))

		var informational []int
		trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
			informational = append(informational, code)
			return nil
		}}
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"axonhub"}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, `{"results":["ok"]}`, string(body))
		require.Empty(t, informational, "failed key's 103 informational headers must not leak")
		require.EqualValues(t, 2, calls.Load())

		mu.Lock()
		require.Len(t, seenKeys, 2)
		keyA, keyB := seenKeys[0], seenKeys[1]
		mu.Unlock()
		require.NotEqual(t, keyA, keyB)

		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Equal(t, channel.StatusEnabled, saved.Status, "channel 403 profile must not fire on key-only classified 403")
		require.Len(t, saved.DisabledAPIKeys, 1)
		require.Equal(t, keyA, saved.DisabledAPIKeys[0].Key)
		require.Equal(t, http.StatusForbidden, saved.DisabledAPIKeys[0].ErrorCode)
		inFlightA, _, _, _ := f.channels.CredentialRuntimeMetrics(f.channelID, keyA)
		inFlightB, _, _, _ := f.channels.CredentialRuntimeMetrics(f.channelID, keyB)
		require.Zero(t, inFlightA)
		require.Zero(t, inFlightB)
	})

	t.Run("TEAM_BLOCKED on sole active key applies key disablement and last-key channel consequence", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"sole-key"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"sole"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, `{"error":"Blocked","tag":"TEAM_BLOCKED"}`, string(body))
		require.EqualValues(t, 1, calls.Load())

		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Equal(t, channel.StatusDisabled, saved.Status)
		require.NotNil(t, saved.ErrorMessage)
		require.Contains(t, *saved.ErrorMessage, "All API keys disabled")
		require.Len(t, saved.DisabledAPIKeys, 1)
		require.Equal(t, "sole-key", saved.DisabledAPIKeys[0].Key)
	})

	t.Run("non-matching tags, prose-only text, HTML, oversize 403, wrong path, and 200 /contents statuses[] stay untouched", func(t *testing.T) {
		oversizeBody := `{"tag":"TEAM_BLOCKED","pad":"` + strings.Repeat("a", rawHTTPErrorInspectLimit+128) + `"}`
		cases := []struct {
			name        string
			path        string
			status      int
			contentType string
			body        string
		}{
			{
				name:        "FEATURE_DISABLED tag",
				path:        "/search",
				status:      http.StatusForbidden,
				contentType: "application/json",
				body:        `{"error":"Feature disabled","tag":"FEATURE_DISABLED"}`,
			},
			{
				name:        "prose-only TEAM_BLOCKED without structured tag",
				path:        "/search",
				status:      http.StatusForbidden,
				contentType: "application/json",
				body:        `{"error":"TEAM_BLOCKED mentioned only in prose","message":"TEAM_BLOCKED"}`,
			},
			{
				name:        "HTML 403 body",
				path:        "/search",
				status:      http.StatusForbidden,
				contentType: "text/html",
				body:        `<html><body>403 TEAM_BLOCKED</body></html>`,
			},
			{
				name:        "oversized 403 body beyond 64 KiB cap",
				path:        "/search",
				status:      http.StatusForbidden,
				contentType: "application/json",
				body:        oversizeBody,
			},
			{
				name:        "wrong path /contents with TEAM_BLOCKED 403",
				path:        "/contents",
				status:      http.StatusForbidden,
				contentType: "application/json",
				body:        `{"error":"Blocked","tag":"TEAM_BLOCKED"}`,
			},
			{
				name:        "200 OK /contents with nested statuses[] errors",
				path:        "/contents",
				status:      http.StatusOK,
				contentType: "application/json",
				body:        `{"results":[],"statuses":[{"id":"https://example.com","status":"error","error":{"tag":"TEAM_BLOCKED"}}]}`,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", tc.contentType)
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer upstream.Close()

				f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
					s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
				})
				ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))
				require.NoError(t, f.channels.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
					Enabled: true,
					AutoDisableChannel: biz.AutoDisableChannel{
						Enabled:  true,
						Statuses: []biz.AutoDisableChannelStatus{{Status: http.StatusForbidden, Times: 1}},
					},
				}))

				req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d%s", f.server.URL, f.channelID, tc.path), strings.NewReader(`{"query":"q"}`))
				require.NoError(t, err)
				resp, err := rawHTTPRetryClient(t).Do(req)
				require.NoError(t, err)
				got, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())

				require.Equal(t, tc.status, resp.StatusCode)
				require.Equal(t, tc.body, string(got))
				require.EqualValues(t, 1, calls.Load())

				saved := f.db.Channel.GetX(ctx, f.channelID)
				require.Equal(t, channel.StatusEnabled, saved.Status)
				require.Empty(t, saved.DisabledAPIKeys)
			})
		}
	})

	t.Run("classified 403 then B returns 429 stops with NO 3rd call", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		var seenKeys []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			mu.Lock()
			seenKeys = append(seenKeys, r.Header.Get("x-api-key"))
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
				return
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2", "key-3"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"q"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
		require.Equal(t, `{"error":"rate limited"}`, string(body))
		require.EqualValues(t, 2, calls.Load(), "403 -> 429 must never attempt a 3rd key")

		mu.Lock()
		keyA, keyB := seenKeys[0], seenKeys[1]
		mu.Unlock()

		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Len(t, saved.DisabledAPIKeys, 1)
		require.Equal(t, keyA, saved.DisabledAPIKeys[0].Key)
		_, _, _, cooldownB := f.channels.CredentialRuntimeMetrics(f.channelID, keyB)
		require.True(t, cooldownB.After(time.Now()), "Key B's 429 must still record its own cooldown")
	})

	t.Run("classified 403 with AllowReplay=false attributes failure without replaying POST", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(false))
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"q"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.EqualValues(t, 1, calls.Load())
		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Len(t, saved.DisabledAPIKeys, 1)
	})

	t.Run("truncated winning B body after classified 403 is attributed to B as 502 with no 3rd call", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		var seenKeys []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			mu.Lock()
			seenKeys = append(seenKeys, r.Header.Get("x-api-key"))
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
				return
			}
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2", "key-3"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))
		require.NoError(t, f.channels.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
			Enabled: true,
			AutoDisableChannel: biz.AutoDisableChannel{
				Enabled:  true,
				Statuses: []biz.AutoDisableChannelStatus{{Status: http.StatusBadGateway, Times: 1}},
			},
		}))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"q"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, readErr := io.ReadAll(resp.Body)
		require.Error(t, readErr)
		_ = resp.Body.Close()

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.EqualValues(t, 2, calls.Load(), "truncated winning body must not trigger a 3rd attempt")
		mu.Lock()
		keyA, keyB := seenKeys[0], seenKeys[1]
		mu.Unlock()

		saved := f.db.Channel.GetX(ctx, f.channelID)
		disabledByKey := lo.SliceToMap(saved.DisabledAPIKeys, func(dk objects.DisabledAPIKey) (string, int) { return dk.Key, dk.ErrorCode })
		require.Equal(t, http.StatusForbidden, disabledByKey[keyA])
		require.Equal(t, http.StatusBadGateway, disabledByKey[keyB])
	})

	t.Run("opaque HTML 403 does not trigger inherited generic status>=400 Expr profile", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<html><body>Cloudflare 403</body></html>`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, nil)
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))
		require.NoError(t, f.channels.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
			Enabled: true,
			FailurePolicy: objects.FailurePolicy{
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:                "generic-4xx",
					Name:              "Generic 4xx",
					Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					ConditionCombiner: objects.FailurePolicyConditionCombinerAnd,
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						Expr: `status >= 400`,
					},
					Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
				}},
			},
		}))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, `<html><body>Cloudflare 403</body></html>`, string(body))
		require.EqualValues(t, 1, calls.Load())
		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Equal(t, channel.StatusEnabled, saved.Status)
		require.Empty(t, saved.DisabledAPIKeys)
	})

	t.Run("key-only classified 403 never executes disable_channel inside a KeyProfile", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = &objects.ChannelFailurePolicy{
				Mode: objects.ChannelFailurePolicyModeOverride,
				KeyProfiles: []objects.FailurePolicyProfile{{
					ID:                "exa-with-disable-channel",
					Name:              "Exa Key Profile With Disable Channel",
					AllowReplay:       lo.ToPtr(true),
					Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
					ConditionCombiner: objects.FailurePolicyConditionCombinerAnd,
					Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
						StatusCodes: []int{http.StatusForbidden},
						Expr:        `status == 403 && method == "POST" && path == "/search" && errorTag == "TEAM_BLOCKED"`,
					},
					Actions: []objects.FailurePolicyAction{
						{Type: objects.FailurePolicyActionDisableKey},
						{Type: objects.FailurePolicyActionDisableChannel},
					},
				}},
			}
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"q":"x"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusOK, resp.StatusCode)
		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Equal(t, channel.StatusEnabled, saved.Status, "key-only classified 403 must not execute disable_channel on a multi-key channel")
		require.Len(t, saved.DisabledAPIKeys, 1)
	})

	t.Run("trailing space path /search%20 does not alias exact /search rule", func(t *testing.T) {
		var calls atomic.Int32
		var seenPath string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			seenPath = r.URL.Path
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search%%20", f.server.URL, f.channelID), strings.NewReader(`{"q":"x"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "/search ", seenPath)
		require.EqualValues(t, 1, calls.Load(), "/search%20 must not match exact /search rule or replay")
		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Empty(t, saved.DisabledAPIKeys)
	})

	t.Run("alternate returning 101 Switching Protocols after classified 403 releases lease cleanly", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		var seenKeys []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			mu.Lock()
			seenKeys = append(seenKeys, r.Header.Get("x-api-key"))
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
				return
			}
			conn, rw, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = rw.Flush()
			_ = conn.Close()
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.FailurePolicy = exaTeamBlockedFailurePolicy(lo.ToPtr(true))
		})

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"q":"x"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusBadGateway, resp.StatusCode)
		require.EqualValues(t, 2, calls.Load())
		mu.Lock()
		keyA, keyB := seenKeys[0], seenKeys[1]
		mu.Unlock()
		inFlightA, _, _, _ := f.channels.CredentialRuntimeMetrics(f.channelID, keyA)
		inFlightB, _, _, _ := f.channels.CredentialRuntimeMetrics(f.channelID, keyB)
		require.Zero(t, inFlightA)
		require.Zero(t, inFlightB, "rejected 101 upgrade on alternate must never leak inFlight lease")
	})
}

func TestRawHTTPOpaque403DifferentialRecovery(t *testing.T) {
	t.Run("unconfigured channel keeps 1-call pass-through with neutral lease release", func(t *testing.T) {
		var calls atomic.Int32
		var seenKey string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			seenKey = r.Header.Get("x-api-key")
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"opaque"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, nil)
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, `{"error":"opaque"}`, string(body))
		require.EqualValues(t, 1, calls.Load())

		inFlight, failureRate, latencyMs, cooldown := f.channels.CredentialRuntimeMetrics(f.channelID, seenKey)
		require.Zero(t, inFlight)
		require.Zero(t, failureRate, "opaque 403 must not alter failureRate EWMA")
		require.Zero(t, latencyMs, "opaque 403 must not sample latency EWMA")
		require.True(t, cooldown.IsZero())
	})

	t.Run("configured safe POST /search recovers 403 -> 200 with zero penalty on A", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		var seenKeys []string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			mu.Lock()
			seenKeys = append(seenKeys, r.Header.Get("x-api-key"))
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"opaque"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"recovered":true}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.RawHTTP.Opaque403Recovery = lo.ToPtr(true)
			s.RawHTTP.SafeReplayPaths = []string{"/search"}
		})
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"diff"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, `{"recovered":true}`, string(body))
		require.EqualValues(t, 2, calls.Load())

		mu.Lock()
		keyA := seenKeys[0]
		mu.Unlock()
		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Empty(t, saved.DisabledAPIKeys, "opaque differential 403 -> 200 must never durably penalize A")
		inFlight, failureRate, latencyMs, cooldown := f.channels.CredentialRuntimeMetrics(f.channelID, keyA)
		require.Zero(t, inFlight)
		require.Zero(t, failureRate)
		require.Zero(t, latencyMs)
		require.True(t, cooldown.IsZero())
	})

	t.Run("opaque 403 -> 200 forwards B's 103 Early Hints while discarding A's 103", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.Header().Set("Link", "</from-a>; rel=preload")
				w.WriteHeader(http.StatusEarlyHints)
				w.Header().Del("Link")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"opaque"}`))
				return
			}
			w.Header().Set("Link", "</from-b>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.Header().Del("Link")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"from":"b"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.RawHTTP.Opaque403Recovery = lo.ToPtr(true)
			s.RawHTTP.SafeReplayPaths = []string{"/search"}
		})

		var links []string
		trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, h textproto.MIMEHeader) error {
			if code == http.StatusEarlyHints {
				links = append(links, h.Get("Link"))
			}
			return nil
		}}
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"q":"hints"}`))
		require.NoError(t, err)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))

		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, `{"from":"b"}`, string(body))
		require.Equal(t, []string{"</from-b>; rel=preload"}, links, "winning B's 103 must be forwarded without leaking A's 103")
	})

	t.Run("disallowed POST /create never replays even when Opaque403Recovery is enabled", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"opaque"}`))
		}))
		defer upstream.Close()

		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, func(s *objects.ChannelSettings) {
			s.RawHTTP.Opaque403Recovery = lo.ToPtr(true)
			s.RawHTTP.SafeReplayPaths = []string{"/search"}
		})

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/create", f.server.URL, f.channelID), strings.NewReader(`{"charge":10}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.EqualValues(t, 1, calls.Load())
	})

	t.Run("opaque 403 then B returns non-2xx or transport error preserves A unchanged with no 3rd call", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			bStatus   int
			bHijack   bool
		}{
			{name: "B 403", bStatus: http.StatusForbidden},
			{name: "B 429", bStatus: http.StatusTooManyRequests},
			{name: "B 503", bStatus: http.StatusServiceUnavailable},
			{name: "B 302", bStatus: http.StatusFound},
			{name: "B 304", bStatus: http.StatusNotModified},
			{name: "B transport error", bHijack: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var calls atomic.Int32
				var mu sync.Mutex
				var seenKeys []string
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					mu.Lock()
					seenKeys = append(seenKeys, r.Header.Get("x-api-key"))
					mu.Unlock()
					_, _ = io.Copy(io.Discard, r.Body)
					if n == 1 {
						w.Header().Set("X-Origin-A", "keep-me")
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte("original-A-403-payload"))
						return
					}
					if tc.bHijack {
						conn, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = conn.Close()
						return
					}
					if tc.bStatus == http.StatusFound {
						w.Header().Set("Location", "https://example.invalid/redirect")
					}
					w.Header().Set("X-Origin-B", "must-not-leak")
					w.WriteHeader(tc.bStatus)
					if tc.bStatus != http.StatusNotModified {
						_, _ = w.Write([]byte("alternate-B-payload"))
					}
				}))
				defer upstream.Close()

				f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2", "key-3"}, func(s *objects.ChannelSettings) {
					s.RawHTTP.Opaque403Recovery = lo.ToPtr(true)
					s.RawHTTP.SafeReplayPaths = []string{"/search"}
				})
				ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))

				req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"q"}`))
				require.NoError(t, err)
				resp, err := rawHTTPRetryClient(t).Do(req)
				require.NoError(t, err)
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				require.NoError(t, resp.Body.Close())

				require.Equal(t, http.StatusForbidden, resp.StatusCode)
				require.Equal(t, "keep-me", resp.Header.Get("X-Origin-A"))
				require.Empty(t, resp.Header.Get("X-Origin-B"))
				require.Equal(t, "original-A-403-payload", string(body))
				require.EqualValues(t, 2, calls.Load(), "must never try a 3rd key after opaque 403")

				mu.Lock()
				keyA := seenKeys[0]
				mu.Unlock()
				saved := f.db.Channel.GetX(ctx, f.channelID)
				require.Empty(t, saved.DisabledAPIKeys)
				inFlightA, failureRateA, latencyMsA, cooldownA := f.channels.CredentialRuntimeMetrics(f.channelID, keyA)
				require.Zero(t, inFlightA)
				require.Zero(t, failureRateA)
				require.Zero(t, latencyMsA)
				require.True(t, cooldownA.IsZero())
			})
		}
	})
}

func TestRawHTTPStrictRetrySelectionAndProxyLimitOne(t *testing.T) {
	t.Run("exhausted and cooling pool stops immediately instead of looping 432s", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(432)
			_, _ = w.Write([]byte(`{"error":"quota exhausted"}`))
		}))
		defer upstream.Close()

		zero := 0.0
		limit := 1000.0
		future := time.Now().Add(time.Hour)
		f := newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2", "key-3", "key-4"}, func(s *objects.ChannelSettings) {
			s.KeyHealthCheck = &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{
					{ID: objects.ChannelAPIKeyFingerprint("key-1"), BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{Limit: &limit, Remaining: &zero, ResetAt: &future}},
					{ID: objects.ChannelAPIKeyFingerprint("key-2"), BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{Limit: &limit, Remaining: &zero, ResetAt: &future}},
					{ID: objects.ChannelAPIKeyFingerprint("key-3"), BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{Limit: &limit, Remaining: &zero, ResetAt: &future}},
					{ID: objects.ChannelAPIKeyFingerprint("key-4"), BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{Limit: &limit, Remaining: &zero, ResetAt: &future}},
				},
			}
		})

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"query":"tavily"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, 432, resp.StatusCode)
		require.Equal(t, `{"error":"quota exhausted"}`, string(body))
		require.EqualValues(t, 1, calls.Load(), "strict retry selection (allowSoftFallback=false) must not loop through exhausted keys")
	})

	t.Run("shared proxy with MaxConcurrent=1 switches A -> B on classified and opaque 403 without self-deadlock", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"via":"proxy-limit-1"}`))
		}))
		defer upstream.Close()

		// Lightweight HTTP forward proxy pointing to upstream.
		proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outReq, err := http.NewRequestWithContext(r.Context(), r.Method, r.RequestURI, r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			outReq.Header = r.Header.Clone()
			outResp, err := http.DefaultTransport.RoundTrip(outReq)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer outResp.Body.Close()
			for k, vals := range outResp.Header {
				for _, v := range vals {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(outResp.StatusCode)
			_, _ = io.Copy(w, outResp.Body)
		}))
		defer proxyServer.Close()

		proxyURL, err := url.Parse(proxyServer.URL)
		require.NoError(t, err)

		gin.SetMode(gin.TestMode)
		db := enttest.NewEntClient(t, "sqlite3", "file:raw-http-proxy-limit-"+uuid.NewString()+"?mode=memory&_fk=1")
		channels := biz.NewChannelServiceForTest(db)
		defer func() {
			channels.Stop()
			_ = db.Close()
		}()

		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))
		managedProxy := db.OutboundProxy.Create().
			SetName("single-slot-proxy").
			SetProxyURL(proxyURL.String()).
			SetStatus(outboundproxy.StatusEnabled).
			SetMaxConcurrent(1).
			SaveX(ctx)

		settings := &objects.ChannelSettings{
			KeySelection:  &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
			Proxy:         &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
			RawHTTP:       &objects.ChannelRawHTTP{Enabled: true, Resource: "exa-search", AuthType: "header", AuthHeader: "x-api-key"},
			FailurePolicy: exaTeamBlockedFailurePolicy(lo.ToPtr(true)),
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:               objects.ChannelAPIKeyFingerprint("proxy-key-a"),
					EgressMode:       objects.CredentialEgressModePinnedProxy,
					OutboundProxyID:  lo.ToPtr(managedProxy.ID),
					EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
				},
				{
					ID:               objects.ChannelAPIKeyFingerprint("proxy-key-b"),
					EgressMode:       objects.CredentialEgressModePinnedProxy,
					OutboundProxyID:  lo.ToPtr(managedProxy.ID),
					EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
				},
			},
		}
		row := db.Channel.Create().
			SetName("Exa Proxy Limit 1").
			SetType(channel.TypeOpenai).
			SetStatus(channel.StatusEnabled).
			SetBaseURL(upstream.URL).
			SetCredentials(objects.ChannelCredentials{APIKeys: []string{"proxy-key-a", "proxy-key-b"}}).
			SetSupportedModels([]string{"exa-search"}).
			SetDefaultTestModel("exa-search").
			SetSettings(settings).
			SaveX(ctx)

		handler := NewRawHTTPHandlers(RawHTTPHandlersParams{ChannelService: channels, Ent: db})
		router := gin.New()
		router.Use(func(c *gin.Context) {
			reqCtx := ent.NewContext(authz.WithTestBypass(c.Request.Context()), db)
			reqCtx = contexts.WithAPIKey(reqCtx, &ent.APIKey{ID: 1, ProjectID: 1, Scopes: []string{string(scopes.ScopeWriteRequests)}})
			c.Request = c.Request.WithContext(reqCtx)
		})
		router.Any("/raw/:channelID/*path", handler.Proxy)
		server := httptest.NewServer(router)
		defer server.Close()

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", server.URL, row.ID), strings.NewReader(`{"query":"proxy"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, `{"via":"proxy-limit-1"}`, string(body))
		require.EqualValues(t, 2, calls.Load())
		inFlightA, _, _, _ := channels.CredentialRuntimeMetrics(row.ID, "proxy-key-a")
		inFlightB, _, _, _ := channels.CredentialRuntimeMetrics(row.ID, "proxy-key-b")
		require.Zero(t, inFlightA)
		require.Zero(t, inFlightB)

		// Also verify opaque 403 differential recovery on the same single-slot proxy.
		require.NoError(t, channels.EnableAllAPIKeys(ctx, row.ID))
		row2 := db.Channel.GetX(ctx, row.ID)
		nextSettings := *row2.Settings
		rawCopy := *row2.Settings.RawHTTP
		rawCopy.Opaque403Recovery = lo.ToPtr(true)
		rawCopy.SafeReplayPaths = []string{"/search"}
		nextSettings.RawHTTP = &rawCopy
		nextSettings.FailurePolicy = nil
		db.Channel.UpdateOneID(row.ID).SetSettings(&nextSettings).SaveX(ctx)
		calls.Store(0)

		req2, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", server.URL, row.ID), strings.NewReader(`{"query":"opaque-proxy"}`))
		require.NoError(t, err)
		resp2, err := rawHTTPRetryClient(t).Do(req2)
		require.NoError(t, err)
		body2, err := io.ReadAll(resp2.Body)
		require.NoError(t, err)
		require.NoError(t, resp2.Body.Close())

		require.Equal(t, http.StatusOK, resp2.StatusCode)
		require.Equal(t, `{"via":"proxy-limit-1"}`, string(body2))
		require.EqualValues(t, 2, calls.Load())
		inFlightA, _, _, _ = channels.CredentialRuntimeMetrics(row.ID, "proxy-key-a")
		inFlightB, _, _, _ = channels.CredentialRuntimeMetrics(row.ID, "proxy-key-b")
		require.Zero(t, inFlightA)
		require.Zero(t, inFlightB)
	})

	t.Run("retry skips alternate key disabled while attempt A is in flight", func(t *testing.T) {
		var calls atomic.Int32
		var mu sync.Mutex
		var seenKeys []string
		var f *rawHTTPFullFixture
		var disableOnce sync.Once

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			key := r.Header.Get("x-api-key")
			mu.Lock()
			seenKeys = append(seenKeys, key)
			mu.Unlock()
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				// Disable the other key in DB while A is in flight.
				disableOnce.Do(func() {
					otherKey := "key-2"
					if key == "key-2" {
						otherKey = "key-1"
					}
					bgCtx := authz.WithTestBypass(ent.NewContext(context.Background(), f.db))
					require.NoError(t, f.channels.DisableAPIKey(bgCtx, f.channelID, otherKey, http.StatusUnauthorized, "disabled mid-flight"))
				})
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"bad key a"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()

		f = newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, nil)
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"q":"midflight"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "mid-flight disabled alternate must not be selected")
		require.EqualValues(t, 1, calls.Load())
		mu.Lock()
		require.Len(t, seenKeys, 1)
		mu.Unlock()
	})

	t.Run("retry skips alternate key deleted while attempt A is in flight", func(t *testing.T) {
		var calls atomic.Int32
		var f *rawHTTPFullFixture
		var deleteOnce sync.Once

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			key := r.Header.Get("x-api-key")
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				deleteOnce.Do(func() {
					otherKey := "key-2"
					if key == "key-2" {
						otherKey = "key-1"
					}
					bgCtx := authz.WithTestBypass(ent.NewContext(context.Background(), f.db))
					_, err := f.channels.DeleteChannelAPIKey(bgCtx, f.channelID, objects.ChannelAPIKeyFingerprint(otherKey))
					require.NoError(t, err)
				})
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"bad key a"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()

		f = newRawHTTPFullFixture(t, upstream, []string{"key-1", "key-2"}, nil)
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{"q":"deleted"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		_, _ = io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "mid-flight deleted alternate must not be selected")
		require.EqualValues(t, 1, calls.Load())
	})

	t.Run("route-local classified 403 on dependent channel does not mutate shared credential owner without owner rule", func(t *testing.T) {
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"Blocked","tag":"TEAM_BLOCKED"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		defer upstream.Close()

		gin.SetMode(gin.TestMode)
		db := enttest.NewEntClient(t, "sqlite3", "file:raw-http-shared-owner-"+uuid.NewString()+"?mode=memory&_fk=1")
		channels := biz.NewChannelServiceForTest(db)
		defer func() {
			channels.Stop()
			_ = db.Close()
		}()
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), db))

		owner := db.Channel.Create().
			SetName("Shared Exa Pool Owner").
			SetType(channel.TypeOpenai).
			SetStatus(channel.StatusEnabled).
			SetBaseURL(upstream.URL).
			SetCredentials(objects.ChannelCredentials{APIKeys: []string{"shared-k1", "shared-k2"}}).
			SetSupportedModels([]string{"exa-search"}).
			SetDefaultTestModel("exa-search").
			SetSettings(&objects.ChannelSettings{
				Proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
			}).
			SaveX(ctx)

		dependent := db.Channel.Create().
			SetName("Dependent Exa Route").
			SetType(channel.TypeOpenai).
			SetStatus(channel.StatusEnabled).
			SetBaseURL(upstream.URL).
			SetCredentials(objects.ChannelCredentials{}).
			SetSupportedModels([]string{"exa-search"}).
			SetDefaultTestModel("exa-search").
			SetSettings(&objects.ChannelSettings{
				CredentialSourceChannelID: lo.ToPtr(owner.ID),
				KeySelection:              &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
				Proxy:                     &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
				RawHTTP:                   &objects.ChannelRawHTTP{Enabled: true, Resource: "exa-search", AuthType: "header", AuthHeader: "x-api-key"},
				FailurePolicy:             exaTeamBlockedFailurePolicy(lo.ToPtr(true)),
			}).
			SaveX(ctx)

		handler := NewRawHTTPHandlers(RawHTTPHandlersParams{ChannelService: channels, Ent: db})
		router := gin.New()
		router.Use(func(c *gin.Context) {
			reqCtx := ent.NewContext(authz.WithTestBypass(c.Request.Context()), db)
			reqCtx = contexts.WithAPIKey(reqCtx, &ent.APIKey{ID: 1, ProjectID: 1, Scopes: []string{string(scopes.ScopeWriteRequests)}})
			c.Request = c.Request.WithContext(reqCtx)
		})
		router.Any("/raw/:channelID/*path", handler.Proxy)
		server := httptest.NewServer(router)
		defer server.Close()

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/raw/%d/search", server.URL, dependent.ID), strings.NewReader(`{"q":"shared"}`))
		require.NoError(t, err)
		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, `{"ok":true}`, string(body))
		require.EqualValues(t, 2, calls.Load())

		savedOwner := db.Channel.GetX(ctx, owner.ID)
		require.Empty(t, savedOwner.DisabledAPIKeys, "dependent route's local 403 rule must not disable key on shared credential owner without owner-level rule")
	})
}

func TestRawHTTPClientDisconnectAfter432StillAppliesFailurePolicy(t *testing.T) {
	t.Run("disconnect during 432 response body still disables key and sets cooldown", func(t *testing.T) {
		headersSent := make(chan struct{})
		releaseUpstream := make(chan struct{})

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(432)
			_, _ = w.Write([]byte(`{"error":"plan limit"`))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-headersSent:
			default:
				close(headersSent)
			}
			select {
			case <-releaseUpstream:
			case <-r.Context().Done():
			}
		}))
		defer upstream.Close()
		defer close(releaseUpstream)

		f := newRawHTTPFullFixture(t, upstream, []string{"tavily-key-1"}, nil)
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))
		require.NoError(t, f.channels.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
			Enabled: true,
			AutoDisableChannel: biz.AutoDisableChannel{
				Enabled:  true,
				Statuses: []biz.AutoDisableChannelStatus{{Status: 432, Times: 1}},
			},
		}))

		reqCtx, cancel := context.WithCancel(t.Context())
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), bytes.NewReader([]byte(`{"q":"x"}`)))
		require.NoError(t, err)

		resp, err := rawHTTPRetryClient(t).Do(req)
		require.NoError(t, err)
		require.Equal(t, 432, resp.StatusCode)
		// Disconnect downstream client while response body is still open.
		cancel()
		_ = resp.Body.Close()

		require.Eventually(t, func() bool {
			saved, err := f.db.Channel.Get(ctx, f.channelID)
			if err != nil {
				return false
			}
			return len(saved.DisabledAPIKeys) == 1 && saved.DisabledAPIKeys[0].ErrorCode == 432
		}, 5*time.Second, 20*time.Millisecond, "detached failure policy must still disable key after 432 + client disconnect")

		inFlight, _, _, cooldown := f.channels.CredentialRuntimeMetrics(f.channelID, "tavily-key-1")
		require.Zero(t, inFlight)
		require.True(t, cooldown.After(time.Now()), "432 cooldown must not be erased by downstream disconnect")

		require.Eventually(t, func() bool {
			rows, err := f.db.Request.Query().All(ctx)
			if err != nil || len(rows) == 0 {
				return false
			}
			return rows[0].Status == request.StatusFailed
		}, 5*time.Second, 20*time.Millisecond, "request observation must retain failed status after authoritative 432 + disconnect")
	})

	t.Run("disconnect before upstream response headers imposes zero penalty", func(t *testing.T) {
		arrived := make(chan struct{})
		releaseUpstream := make(chan struct{})

		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-arrived:
			default:
				close(arrived)
			}
			select {
			case <-r.Context().Done():
			case <-releaseUpstream:
			}
		}))
		defer upstream.Close()
		defer close(releaseUpstream)

		f := newRawHTTPFullFixture(t, upstream, []string{"key-cancel-1"}, nil)
		ctx := authz.WithTestBypass(ent.NewContext(t.Context(), f.db))
		require.NoError(t, f.channels.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
			Enabled: true,
			AutoDisableChannel: biz.AutoDisableChannel{
				Enabled:  true,
				Statuses: []biz.AutoDisableChannelStatus{{Status: http.StatusBadGateway, Times: 1}},
			},
		}))

		reqCtx, cancel := context.WithCancel(t.Context())
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, fmt.Sprintf("%s/raw/%d/search", f.server.URL, f.channelID), strings.NewReader(`{}`))
		require.NoError(t, err)

		done := make(chan error, 1)
		go func() {
			resp, err := rawHTTPRetryClient(t).Do(req)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()

		select {
		case <-arrived:
		case err := <-done:
			t.Fatalf("request finished before reaching upstream: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for request to reach upstream")
		}
		cancel()
		require.Error(t, <-done)

		require.Eventually(t, func() bool {
			inFlight, _, _, _ := f.channels.CredentialRuntimeMetrics(f.channelID, "key-cancel-1")
			return inFlight == 0
		}, 5*time.Second, 20*time.Millisecond)

		saved := f.db.Channel.GetX(ctx, f.channelID)
		require.Empty(t, saved.DisabledAPIKeys)
		_, failureRate, latencyMs, cooldown := f.channels.CredentialRuntimeMetrics(f.channelID, "key-cancel-1")
		require.Zero(t, failureRate)
		require.Zero(t, latencyMs)
		require.True(t, cooldown.IsZero())
	})
}
