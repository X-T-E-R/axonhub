package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestCheckIPBlocklistCurrentPolicyWithoutResponseWrite(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ip-policy-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client})
	setBlocked := func(blocked []string) {
		require.NoError(t, system.SetSecuritySettings(ctx, biz.SecuritySettings{BlockedIPs: blocked, CyberSessionBlockTTLSeconds: 600}))
	}
	setBlocked(nil)
	for _, blocked := range []string{"198.51.100.10", "203.0.113.0/24", "192.0.2.30"} {
		t.Run(blocked, func(t *testing.T) {
			setBlocked(nil)
			recorder := httptest.NewRecorder()
			c, engine := gin.CreateTestContext(recorder)
			require.NoError(t, engine.SetTrustedProxies(nil))
			c.Request = httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
			c.Request.RemoteAddr = "198.51.100.10:1234"
			c.Request.Header.Set("X-Forwarded-For", "203.0.113.10, 203.0.113.11")
			c.Request.Header.Set("X-Real-IP", "192.0.2.30")
			WithIPBlocklist(system)(c)
			require.False(t, c.Writer.Written())
			frame := c.Copy() // Copy has no writable underlying HTTP response.
			frame.Request = c.Request.Clone(contexts.NewRequestScope(c.Request.Context()))
			require.NoError(t, CheckIPBlocklist(frame))
			setBlocked([]string{blocked})
			require.ErrorContains(t, CheckIPBlocklist(frame), "IP address is blocked")
			require.False(t, c.Writer.Written())
			require.Zero(t, recorder.Body.Len())
			// The global candidates do not change per-key direct-peer semantics.
			require.True(t, isAllowedRemoteIP(frame, []string{"198.51.100.10"}))
			require.False(t, isAllowedRemoteIP(frame, []string{"203.0.113.0/24", "192.0.2.30"}))
			setBlocked(nil)
			require.NoError(t, CheckIPBlocklist(frame))
		})
	}
}
