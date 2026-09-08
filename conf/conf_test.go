package conf

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
)

func TestLoaderReloadUsesFreshViper(t *testing.T) {
	configFile := writeTestConfig(t, `
log:
  level: info
`)
	loader := &Loader{configFile: configFile}

	initial, err := loader.Reload()
	if err != nil {
		t.Fatalf("initial Reload() error = %v", err)
	}
	if initial.Log.Level != zapcore.InfoLevel {
		t.Fatalf("initial log level = %s, want %s", initial.Log.Level, zapcore.InfoLevel)
	}

	if err := os.WriteFile(configFile, []byte(`
log:
  level: debug
`), 0o600); err != nil {
		t.Fatalf("rewrite test config: %v", err)
	}

	reloaded, err := loader.Reload()
	if err != nil {
		t.Fatalf("reloaded Reload() error = %v", err)
	}
	if reloaded.Log.Level != zapcore.DebugLevel {
		t.Fatalf("reloaded log level = %s, want %s", reloaded.Log.Level, zapcore.DebugLevel)
	}
}

func writeTestConfig(t *testing.T, contents string) string {
	t.Helper()

	configFile := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return configFile
}

func TestSSEKeepAliveConfig(t *testing.T) {
	configFile := writeTestConfig(t, `
server:
  sse_keep_alive:
    enabled: true
    interval: 30s
`)

	cfg, _, err := loadConfig(configFile)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if !cfg.APIServer.SSEKeepAlive.Enabled {
		t.Fatal("SSE keep-alive should be enabled")
	}
	if cfg.APIServer.SSEKeepAlive.Interval != 30*time.Second {
		t.Fatalf("SSE keep-alive interval = %s, want 30s", cfg.APIServer.SSEKeepAlive.Interval)
	}
}

func TestSSEKeepAliveDefaultsToDisabled(t *testing.T) {
	configFile := writeTestConfig(t, "")

	cfg, _, err := loadConfig(configFile)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if cfg.APIServer.SSEKeepAlive.Enabled {
		t.Fatal("SSE keep-alive should default to disabled")
	}
	if cfg.APIServer.SSEKeepAlive.Interval != 15*time.Second {
		t.Fatalf("SSE keep-alive interval = %s, want 15s", cfg.APIServer.SSEKeepAlive.Interval)
	}
}

func TestSSEKeepAliveRejectsNonPositiveInterval(t *testing.T) {
	configFile := writeTestConfig(t, `
server:
  sse_keep_alive:
    enabled: true
    interval: 0s
`)

	_, _, err := loadConfig(configFile)
	if err == nil {
		t.Fatal("loadConfig() should reject a non-positive SSE keep-alive interval")
	}
}

func TestWriterItemLimitDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   int
	}{
		{"default", "", 0},
		{"explicit-zero", "managed_request_body_writer:\n  max_items: 0\n", 0},
		{"explicit-limit", "managed_request_body_writer:\n  max_items: 64\n", 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, _, err := loadConfig(writeTestConfig(t, tc.config))
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.ManagedRequestBodyWriter.MaxItems; got != tc.want {
				t.Fatalf("MaxItems = %d, want %d", got, tc.want)
			}
		})
	}
	t.Run("environment", func(t *testing.T) {
		t.Setenv("AXONHUB_MANAGED_REQUEST_BODY_WRITER_MAX_ITEMS", "7")
		cfg, _, err := loadConfig(writeTestConfig(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ManagedRequestBodyWriter.MaxItems != 7 {
			t.Fatal("environment item limit was lost")
		}
	})
}
