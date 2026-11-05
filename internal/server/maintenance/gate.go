// Package maintenance provides one-way, process-local retirement. Restart is
// required to reopen admission after a maintenance pause or failed drain.
package maintenance

import (
	"context"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

type Snapshot struct {
	Paused           bool `json:"paused"`
	ActiveHTTP       int  `json:"activeHTTP"`
	ActiveTurns      int  `json:"activeTurns"`
	ActiveBackground int  `json:"activeBackground"`
}

type Gate struct {
	mu      sync.Mutex
	state   Snapshot
	paused  chan struct{}
	changed chan struct{}
}

func NewGate() *Gate { return &Gate{paused: make(chan struct{}), changed: make(chan struct{})} }

func (g *Gate) notify() { close(g.changed); g.changed = make(chan struct{}) }

func (g *Gate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.state.Paused {
		g.state.Paused = true
		close(g.paused)
		g.notify()
	}
}

func (g *Gate) Paused() <-chan struct{} {
	if g == nil {
		return nil
	}
	return g.paused
}

func (g *Gate) Snapshot() Snapshot {
	if g == nil {
		return Snapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Enter fences admission and active registration under the same lock. A pause
// cannot observe idle while a request/generation is about to start unregistered.
func (g *Gate) Enter(kind string) (func(), bool) {
	if g == nil {
		return func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state.Paused {
		return nil, false
	}
	g.add(kind, 1)
	g.notify()
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); defer g.mu.Unlock(); g.add(kind, -1); g.notify() }) }, true
}

func (g *Gate) add(kind string, n int) {
	switch kind {
	case "http":
		g.state.ActiveHTTP += n
	case "turn":
		g.state.ActiveTurns += n
	case "background":
		g.state.ActiveBackground += n
	default:
		panic("invalid maintenance activity kind")
	}
}

func (g *Gate) Wait(ctx context.Context) error {
	for {
		g.mu.Lock()
		s, changed := g.state, g.changed
		g.mu.Unlock()
		if s.ActiveHTTP == 0 && s.ActiveTurns == 0 && s.ActiveBackground == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Health stays observable; every business/admin route is fenced. Local Unix
// maintenance commands do not traverse this public HTTP middleware.
func (g *Gate) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/health" {
			c.Next()
			return
		}
		done, ok := g.Enter("http")
		if !ok {
			c.Header("Cache-Control", "no-store")
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"execution": "not_executed", "error": gin.H{"type": "server_error", "code": "node_draining", "message": "This node is retiring. The request was not executed."}})
			return
		}
		defer done()
		c.Next()
	}
}
