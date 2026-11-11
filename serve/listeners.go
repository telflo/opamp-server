package serve

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/netutil"

	"github.com/telflo/telflo-opamp-server/internal/shutdown"
)

// The listeners' lifecycle: open every listener or none, serve each
// component's handler, and stop them together, gracefully, after the
// drain. It knows no component: each listener is a listenerSpec.

const (
	// readHeaderTimeout bounds a client that opens a connection and stalls.
	readHeaderTimeout = 10 * time.Second
	// readTimeout bounds reading a whole request, the largest body included.
	readTimeout = 30 * time.Second
	// writeTimeout bounds writing a reply.
	writeTimeout = 30 * time.Second
	// idleTimeout closes a kept-alive connection nobody uses.
	idleTimeout = 120 * time.Second

	// ShutdownTimeout is how long in-flight requests get once serve is told
	// to stop (SIGTERM from compose or Kubernetes, or SIGINT). Requests
	// still running after it are cut off.
	ShutdownTimeout = 10 * time.Second
	// CloseWait is how long serve then waits for the cut-off requests'
	// handlers to return before it closes the pool and destroys the keys.
	CloseWait = 2 * time.Second
)

// listenerSpec is one listener serve opens.
type listenerSpec struct {
	// what is what it serves, as its log lines say ("the management API").
	what string
	// listener and port name it in log lines ("the management listener",
	// "the management port").
	listener, port string
	address        string
	tls            *tls.Config
	handler        http.Handler
	// maxHeaderBytes bounds a request's headers (0: net/http's default,
	// 1 MiB), and maxConnections the connections open at once (0: no cap).
	maxHeaderBytes, maxConnections int
	// attrs are the serving line's attributes after its address and TLS.
	attrs []any
}

// lifecycle is what serveListeners tells its caller: serving once every
// listener is open, draining once it is told to stop, before any listener
// stops; and how long it then keeps the listeners open (drain, on the
// timer after: time.After when nil, a test's channel otherwise).
type lifecycle struct {
	serving  func()
	draining func()
	drain    time.Duration
	after    func(time.Duration) <-chan time.Time
}

// served is one listener's Serve returning.
type served struct {
	index int
	err   error
}

// serveListeners opens every listener (or none: one that can't open closes
// the others and refuses), serves each spec's handler on its own until ctx
// ends or one stops on its own, then stops them all, together: in-flight
// requests get shutdownTimeout to finish, and any still running after it
// have their contexts cancelled (which ends their database queries) and
// their connections closed, and their handlers get CloseWait to return. Each
// listener has the timeouts above (readHeaderTimeout to idleTimeout). It
// returns serve's exit code: 0 after a graceful stop, 1 when a listener
// can't open or stops on its own, or when requests had to be cut off.
func serveListeners(ctx context.Context, specs []listenerSpec, hooks lifecycle, logger *slog.Logger, shutdownTimeout time.Duration) int {
	listeners := make([]net.Listener, 0, len(specs))
	for _, spec := range specs {
		listener, err := net.Listen("tcp", spec.address)
		if err != nil {
			for _, open := range listeners {
				_ = open.Close()
			}
			logger.Error("Refusing to start: can't listen on "+spec.port, "error", err.Error())
			return 1
		}
		if spec.maxConnections > 0 {
			// Past the cap, Accept waits until a connection closes.
			listener = netutil.LimitListener(listener, spec.maxConnections)
		}
		listeners = append(listeners, listener)
	}

	// Every request's context derives from requestsCtx, which only a forced
	// stop cancels: a graceful one lets requests finish. It carries itself
	// (shutdown.WithForcedStop), so a delete that outlives its caller (deletes.go)
	// is still cut off with the rest.
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	requestsCtx := shutdown.WithForcedStop(baseCtx)
	running := &inFlight{}
	servers := make([]*http.Server, len(specs))
	done := make(chan served, len(specs))
	for i, spec := range specs {
		servers[i] = &http.Server{
			Handler:           running.track(spec.handler),
			TLSConfig:         spec.tls,
			ReadHeaderTimeout: readHeaderTimeout,
			ReadTimeout:       readTimeout,
			WriteTimeout:      writeTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    spec.maxHeaderBytes,
			BaseContext:       func(net.Listener) context.Context { return requestsCtx },
			// net/http's own complaints (a failed accept, a bad TLS
			// handshake) go to the same JSON log. They never quote a
			// header or a body.
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		go func() {
			var err error
			if spec.tls != nil {
				// The certificate is TLSConfig's, so no file is named here.
				err = servers[i].ServeTLS(listeners[i], "", "")
			} else {
				err = servers[i].Serve(listeners[i])
			}
			done <- served{index: i, err: err}
		}()
		logger.Info("serving "+spec.what,
			append([]any{"address", listeners[i].Addr().String(), "tls", spec.tls != nil}, spec.attrs...)...)
	}
	if hooks.serving != nil {
		hooks.serving()
	}

	code, pending := 0, len(specs)
	select {
	case stopped := <-done:
		pending--
		logger.Error(specs[stopped.index].listener+" stopped", "error", errorText(stopped.err))
		code = 1
	case <-ctx.Done():
	}
	if hooks.draining != nil {
		hooks.draining()
	}
	if code == 0 && hooks.drain > 0 {
		// Told to stop: /ready already answers 503, and the listeners keep
		// serving for the drain (OPAMP_SHUTDOWN_DRAIN_SECONDS), so a load
		// balancer's readiness checks see the 503 and send no new request
		// before the listeners close. A listener that stops on its own
		// meanwhile ends the wait.
		after := hooks.after
		if after == nil {
			after = time.After
		}
		logger.Info("draining: /ready answers 503; the listeners stop after the drain",
			"drainSeconds", int(hooks.drain/time.Second))
		select {
		case <-after(hooks.drain):
		case stopped := <-done:
			pending--
			logger.Error(specs[stopped.index].listener+" stopped", "error", errorText(stopped.err))
			code = 1
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	failed := make([]error, len(servers))
	var shutdowns sync.WaitGroup
	for i, server := range servers {
		shutdowns.Add(1)
		go func() {
			defer shutdowns.Done()
			failed[i] = server.Shutdown(shutdownCtx)
		}()
	}
	shutdowns.Wait()
	cutOff := errors.Join(failed...) != nil
	if cutOff {
		// Requests still running: cut them off rather than hang.
		// Cancelling their contexts ends their queries; Close ends their
		// connections; their handlers get CloseWait to return.
		cancelRequests()
		for _, server := range servers {
			_ = server.Close()
		}
		running.wait(CloseWait)
	}
	for ; pending > 0; pending-- {
		if stopped := <-done; !errors.Is(stopped.err, http.ErrServerClosed) {
			logger.Error(specs[stopped.index].listener+" stopped", "error", errorText(stopped.err))
			code = 1
		}
	}
	if cutOff {
		logger.Error("requests were still running when the shutdown timeout passed; their connections were closed",
			"timeout", shutdownTimeout.String())
		return 1
	}
	if code == 0 {
		logger.Info("stopped")
	}
	return code
}

// errorText is err's text, or "" for none.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// inFlight counts the requests whose handlers are running, so a forced
// stop can wait for them.
type inFlight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

// track counts next's requests while they run.
func (f *inFlight) track(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.n == 0 {
			f.idle = make(chan struct{})
		}
		f.n++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			if f.n--; f.n == 0 {
				close(f.idle)
			}
			f.mu.Unlock()
		}()
		next.ServeHTTP(w, r)
	})
}

// wait waits until no handler runs, or at most limit, and reports which.
func (f *inFlight) wait(limit time.Duration) bool {
	f.mu.Lock()
	if f.n == 0 {
		f.mu.Unlock()
		return true
	}
	idle := f.idle
	f.mu.Unlock()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-idle:
		return true
	case <-timer.C:
		return false
	}
}
