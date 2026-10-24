package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"time"

	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/log"
)

// debugProfileNames are the only profiles served. cmdline, symbol, trace and
// the textual goroutine dump are deliberately absent: they can expose process
// arguments, runtime symbolisation or live call arguments, while allocation and
// CPU profiles are what diagnosing a sustained-CPU regression needs.
var debugProfileNames = []string{"allocs", "heap"}

// DebugListener serves CPU and allocation profiles on a loopback-only address.
// It is disabled unless server.debug_listener is explicitly configured.
type DebugListener struct {
	Addr string

	server   *http.Server
	listener net.Listener
}

// newDebugProfileMux registers a fixed set of profiling handlers. It never
// mutates http.DefaultServeMux, so the public gin engine cannot reach them.
func newDebugProfileMux() *http.ServeMux {
	onlyGET := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", http.MethodGet)
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

				return
			}
			next(w, r)
		}
	}
	mux := http.NewServeMux()
	// The index handler serves any known profile by name, so restrict it to the
	// exact index path to keep unlisted profiles unreachable.
	mux.HandleFunc("/debug/pprof/", onlyGET(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/pprof/" {
			http.NotFound(w, r)

			return
		}
		pprof.Index(w, r)
	}))
	mux.HandleFunc("/debug/pprof/profile", onlyGET(pprof.Profile))
	for _, name := range debugProfileNames {
		mux.Handle("/debug/pprof/"+name, onlyGET(pprof.Handler(name).ServeHTTP))
	}

	return mux
}

// validateDebugAddr accepts only a literal loopback host. Any routable or
// wildcard host is rejected before a socket is opened, so a mistyped value
// fails startup instead of exposing profiles on the network.
func validateDebugAddr(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("debug listener must be host:port: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", errors.New("debug listener port must be numeric")
	}
	if host == "" {
		return "", errors.New("debug listener host is required")
	}
	if strings.EqualFold(host, "localhost") {
		return addr, nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("debug listener host %q is not loopback", host)
	}

	return addr, nil
}

// NewDebugListener starts the optional profiling listener on the application
// lifecycle. An empty configuration returns a disabled listener and opens
// nothing, which keeps the default process surface unchanged.
func NewDebugListener(lc fx.Lifecycle, cfg Config) (*DebugListener, error) {
	value := strings.TrimSpace(cfg.DebugListener)
	if value == "" {
		return &DebugListener{}, nil
	}
	addr, err := validateDebugAddr(value)
	if err != nil {
		return nil, err
	}
	listener := &DebugListener{Addr: addr}
	lc.Append(fx.Hook{
		OnStart: listener.start,
		OnStop:  listener.stop,
	})

	return listener, nil
}

func (d *DebugListener) start(_ context.Context) error {
	listener, err := net.Listen("tcp", d.Addr)
	if err != nil {
		return fmt.Errorf("debug listener listen: %w", err)
	}
	// Defence in depth: reject a loopback name that resolved elsewhere.
	if tcp, ok := listener.Addr().(*net.TCPAddr); !ok || tcp.IP == nil || !tcp.IP.IsLoopback() {
		_ = listener.Close()

		return errors.New("debug listener bound a non-loopback address")
	}
	d.listener = listener
	d.Addr = listener.Addr().String()
	d.server = &http.Server{
		Handler:           newDebugProfileMux(),
		ReadHeaderTimeout: 5 * time.Second,
		// A CPU profile must outlive its sampling window.
		WriteTimeout: 90 * time.Second,
	}
	go func() {
		if serveErr := d.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			log.Warn(context.Background(), "debug listener stopped unexpectedly",
				log.String("addr", d.Addr), log.Cause(serveErr))
		}
	}()
	log.Info(context.Background(), "debug listener started", log.String("addr", d.Addr))

	return nil
}

func (d *DebugListener) stop(ctx context.Context) error {
	if d.server == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := d.server.Shutdown(shutdownCtx); err != nil {
		_ = d.server.Close()

		return nil
	}

	return nil
}
