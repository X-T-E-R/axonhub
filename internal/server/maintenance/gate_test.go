package maintenance

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMaintenanceGateKeepsAcceptedWorkAndRejectsNew(t *testing.T) {
	gate := NewGate()
	done, ok := gate.Enter("http")
	require.True(t, ok)
	turn, ok := gate.Enter("turn")
	require.True(t, ok)
	gate.Pause()
	_, ok = gate.Enter("turn")
	require.False(t, ok)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, gate.Wait(ctx), context.DeadlineExceeded)
	done()
	turn()
	require.NoError(t, gate.Wait(context.Background()))
	require.True(t, gate.Snapshot().Paused)
	r := gin.New()
	r.Use(gate.Middleware())
	r.GET("/health", func(c *gin.Context) { c.Status(200) })
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1beta/models/m:generateContent", "/gemini/v1beta/models/m:streamGenerateContent", "/admin/graphql", "/__axon/admin/channel-test"} {
		r.POST(path, func(c *gin.Context) { t.Error("paused route executed") })
	}
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1beta/models/m:generateContent", "/gemini/v1beta/models/m:streamGenerateContent", "/admin/graphql", "/__axon/admin/channel-test"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		require.Equal(t, 503, w.Code)
		require.Contains(t, w.Body.String(), "not_executed")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	require.Equal(t, 200, w.Code)
}

func TestMaintenanceDrainOneClaimAndWriterFailureKeepsGate(t *testing.T) {
	for _, failed := range []bool{false, true} {
		gate := NewGate()
		var calls atomic.Int32
		local := &Local{Gate: gate, Drain: func(context.Context) (DrainResult, error) {
			calls.Add(1)
			if failed {
				return DrainResult{}, errors.New("fixture")
			}
			return DrainResult{WritersStopped: true}, nil
		}}
		request := func() {
			w := httptest.NewRecorder()
			local.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/drain", nil))
			require.Equal(t, 202, w.Code)
		}
		request()
		require.Eventually(t, func() bool { s := local.Status(); return s.Phase == "drained" || s.Phase == "failed" }, time.Second, time.Millisecond)
		w := httptest.NewRecorder()
		local.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/drain", nil))
		require.Equal(t, 409, w.Code)
		require.Equal(t, int32(1), calls.Load())
		require.True(t, local.Status().Paused)
		if failed {
			require.Equal(t, "failed", local.Status().Phase)
		} else {
			require.True(t, local.Status().Drain.WritersStopped)
		}
	}
}
