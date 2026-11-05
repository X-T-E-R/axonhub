package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/log"
)

type DrainResult struct {
	WritersStopped bool `json:"writersStopped"`
	NodePending    int  `json:"nodePending"`
	GlobalPending  int  `json:"globalPending"`
}
type State struct {
	Snapshot
	Phase     string       `json:"phase"`
	ErrorCode string       `json:"errorCode,omitempty"`
	Drain     *DrainResult `json:"drain,omitempty"`
}

type Local struct {
	Gate         *Gate
	Drain        func(context.Context) (DrainResult, error)
	mu           sync.Mutex
	state        State
	drainClaimed bool
}

func (l *Local) Status() State {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state
	s.Snapshot = l.Gate.Snapshot()
	if s.Phase == "" {
		if s.Paused {
			s.Phase = "paused"
		} else {
			s.Phase = "running"
		}
	}
	return s
}

func (l *Local) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			_ = json.NewEncoder(w).Encode(l.Status())
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/pause":
			l.Gate.Pause()
		case "/drain":
			l.Gate.Pause()
			l.mu.Lock()
			if l.drainClaimed {
				l.mu.Unlock()
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(l.Status())
				return
			}
			l.drainClaimed = true
			l.state.Phase = "draining"
			l.mu.Unlock()
			go l.work()
		default:
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(l.Status())
	})
}

// The operation owns its deadline, not the transient CLI connection. An unknown
// ACK is resolved with status; no operation is replayed or admission reopened.
func (l *Local) work() {
	defer func() {
		if recover() != nil {
			log.Error(context.Background(), "local maintenance operation panicked")
			l.fail("maintenance_panic_keep_gate")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Second)
	defer cancel()
	if err := l.Gate.Wait(ctx); err != nil {
		l.fail("active_drain_timeout_keep_gate")
		return
	}
	result, err := l.Drain(ctx)
	l.mu.Lock()
	l.state.Drain = &result
	l.mu.Unlock()
	if err != nil {
		l.fail("writer_or_pending_drain_failed_keep_gate")
		return
	}
	l.mu.Lock()
	l.state.Phase = "drained"
	l.mu.Unlock()
}
func (l *Local) fail(code string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state.Phase = "failed"
	l.state.ErrorCode = code
}

func (l *Local) Listen(path string) (func() error, error) {
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	server := &http.Server{Handler: l.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		defer func() {
			if recover() != nil {
				log.Error(context.Background(), "local maintenance listener panicked")
			}
		}()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error(context.Background(), "local maintenance listener stopped")
		}
	}()
	return func() error {
		err := server.Close()
		_ = listener.Close() // Also owns the pre-Serve startup/close race.
		return err
	}, nil
}

func Call(ctx context.Context, path, operation string) (State, error) {
	var result State
	if operation != "status" && operation != "pause" && operation != "drain" {
		return result, errors.New("unknown local maintenance operation")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	method := http.MethodPost
	if operation == "status" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://local/"+operation, nil)
	if err != nil {
		return result, err
	}
	response, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if err = json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&result); err != nil {
		return result, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, errors.New("local maintenance claim rejected; observe status")
	}
	return result, nil
}
