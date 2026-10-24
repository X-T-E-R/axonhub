package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
)

type stubLifecycle struct {
	starters []func(context.Context) error
	stoppers []func(context.Context) error
}

func (s *stubLifecycle) Append(hook fx.Hook) {
	if hook.OnStart != nil {
		s.starters = append(s.starters, hook.OnStart)
	}
	if hook.OnStop != nil {
		s.stoppers = append(s.stoppers, hook.OnStop)
	}
}

func TestDebugListenerDisabledOpensNothing(t *testing.T) {
	t.Parallel()

	lc := &stubLifecycle{}
	listener, err := NewDebugListener(lc, Config{})
	if err != nil {
		t.Fatalf("empty configuration must not fail: %v", err)
	}
	if listener == nil || listener.Addr != "" {
		t.Fatalf("disabled listener must stay empty, got %+v", listener)
	}
	if len(lc.starters) != 0 || len(lc.stoppers) != 0 {
		t.Fatalf("disabled listener registered lifecycle hooks: %d/%d", len(lc.starters), len(lc.stoppers))
	}
}

func TestDebugListenerRejectsNonLoopback(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{
		"0.0.0.0:6060",
		":6060",
		"[::]:6060",
		"192.0.2.10:6060",
		"example.com:6060",
		"localhost:6060",
		"127.0.0.2:6060",
		"[::1]:6060",
		"127.0.0.1",
		"127.0.0.1:http",
		"127.0.0.1:70000",
	} {
		lc := &stubLifecycle{}
		if _, err := NewDebugListener(lc, Config{DebugListener: addr}); err == nil {
			t.Fatalf("%q must be rejected", addr)
		}
		if len(lc.starters) != 0 {
			t.Fatalf("%q registered a start hook despite rejection", addr)
		}
	}
}

func TestDebugListenerServesProfilesOnlyOnLoopback(t *testing.T) {
	lc := &stubLifecycle{}
	listener, err := NewDebugListener(lc, Config{DebugListener: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("loopback listener rejected: %v", err)
	}
	if len(lc.starters) != 1 || len(lc.stoppers) != 1 {
		t.Fatalf("loopback listener must register one start/stop hook, got %d/%d", len(lc.starters), len(lc.stoppers))
	}
	ctx := context.Background()
	if err := lc.starters[0](ctx); err != nil {
		t.Fatalf("start loopback listener: %v", err)
	}
	t.Cleanup(func() { _ = lc.stoppers[0](context.Background()) })

	host, _, err := net.SplitHostPort(listener.Addr)
	if err != nil || host != "127.0.0.1" {
		t.Fatalf("listener exposed a non-loopback address: %q (%v)", listener.Addr, err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	get := func(path string) (int, string) {
		resp, err := client.Get("http://" + listener.Addr + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))

		return resp.StatusCode, string(body)
	}

	if code, _ := get("/debug/pprof/"); code != http.StatusOK {
		t.Fatalf("index status %d", code)
	}
	if code, body := get("/debug/runtime?ignored=not-a-response-field"); code != http.StatusOK {
		t.Fatalf("runtime statistics status %d", code)
	} else {
		var stats map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &stats); err != nil {
			t.Fatalf("runtime statistics JSON: %v", err)
		}
		if len(stats) != 9 || stats["totalAllocBytes"] == nil || stats["heapAllocBytes"] == nil || stats["numGC"] == nil || stats["runtimeCPUCounters"] == nil || strings.Contains(body, "not-a-response-field") {
			t.Fatal("runtime statistics must expose only aggregate allocation/GC fields")
		}
		var counters map[string]float64
		if err := json.Unmarshal(stats["runtimeCPUCounters"], &counters); err != nil || len(counters) != 3 {
			t.Fatal("runtime GC CPU counters unavailable")
		}
	}
	if code, body := get("/debug/pprof/allocs?debug=1"); code != http.StatusOK || !strings.Contains(body, "heap profile:") {
		t.Fatalf("allocs status %d body %q", code, body)
	}
	if code, _ := get("/debug/pprof/heap"); code != http.StatusOK {
		t.Fatalf("heap status %d", code)
	}
	if code, _ := get("/debug/pprof/profile?seconds=1"); code != http.StatusOK {
		t.Fatalf("cpu profile status %d", code)
	}
	// Profiles outside the allow-list must stay unreachable.
	for _, path := range []string{"/debug/pprof/goroutine", "/debug/pprof/cmdline", "/debug/pprof/trace", "/debug/pprof/symbol"} {
		if code, _ := get(path); code == http.StatusOK {
			t.Fatalf("%s must not be served", path)
		}
	}
	resp, err := client.Post("http://"+listener.Addr+"/debug/pprof/heap", "text/plain", strings.NewReader(""))
	if err != nil {
		t.Fatalf("POST heap: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST must be rejected, got %d", resp.StatusCode)
	}
}

func TestDebugListenerStopsWithLifecycle(t *testing.T) {
	lc := &stubLifecycle{}
	listener, err := NewDebugListener(lc, Config{DebugListener: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("loopback listener rejected: %v", err)
	}
	ctx := context.Background()
	if err := lc.starters[0](ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	addr := listener.Addr
	if err := lc.stoppers[0](ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	conn, dialErr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Fatalf("listener still accepting after shutdown on %s", addr)
	}
}
