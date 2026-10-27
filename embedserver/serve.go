package embedserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/daemon"
)

// Options configures the foreground server. DataDir defaults to DefaultDir,
// Listen defaults to 127.0.0.1:0, and Library defaults beside the executable.
// Runtime records use the kit-embed prefix in DataDir.
type Options struct{ DataDir, Listen, Library string }

// DefaultDir returns the shared per-user embeddings data directory.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kenn", "embeddings"), nil
}

// Serve downloads and verifies the model, then serves until cancellation or
// POST /api/shutdown. All requests drain before native resources are released.
func Serve(ctx context.Context, opts Options) error { return serve(ctx, opts, loadModel) }

func serve(ctx context.Context, opts Options, load func(context.Context, string, string) (engine, error)) (err error) {
	if opts.DataDir == "" {
		opts.DataDir, err = DefaultDir()
		if err != nil {
			return err
		}
	}
	opts.DataDir, err = filepath.Abs(opts.DataDir)
	if err != nil {
		return err
	}
	ep, err := daemon.ParseEndpoint(opts.Listen, daemon.ParseEndpointOptions{DefaultTCPAddress: "127.0.0.1:0", TCPPolicy: daemon.RequireLoopback})
	if err != nil {
		return err
	}
	if ep.IsUnix() {
		return errors.New("embeddings requires a loopback TCP endpoint")
	}
	store := daemon.RuntimeStore{Dir: opts.DataDir, Prefix: "kit-embed"}
	release, err := store.AcquireOwnerLock(ctx)
	if err != nil {
		return err
	}
	defer release()
	proof, err := ensureProof(opts.DataDir)
	if err != nil {
		return err
	}
	if opts.Library == "" {
		executable, e := os.Executable()
		if e != nil {
			return e
		}
		opts.Library = filepath.Join(filepath.Dir(executable), libraryName())
	}
	opts.Library, err = filepath.Abs(opts.Library)
	if err != nil {
		return err
	}
	engine, err := load(ctx, opts.DataDir, opts.Library)
	if err != nil {
		return err
	}
	var path string
	defer func() {
		err = errors.Join(err, engine.Close())
		if path != "" {
			err = errors.Join(err, os.Remove(path))
		}
	}()
	listener, err := ep.ListenContext(ctx)
	if err != nil {
		return err
	}
	defer listener.Close()
	ep.Address = listener.Addr().String()
	record := daemon.NewRuntimeRecord("kit-embed", "1", ep)
	ping, err := proof.NewPingHandler(record)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	embeddings := &handler{engine: engine, gate: make(chan struct{}, 1)}
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: time.Minute, BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/ping":
				ping.ServeHTTP(w, r)
			case "/api/shutdown", "/v1/embeddings":
				if r.Method != http.MethodPost {
					w.Header().Set("Allow", "POST")
					http.Error(w, "POST required", http.StatusMethodNotAllowed)
					return
				}
				if r.URL.Path == "/api/shutdown" {
					w.WriteHeader(http.StatusAccepted)
					cancel()
				} else {
					embeddings.ServeHTTP(w, r)
				}
			default:
				http.NotFound(w, r)
			}
		}),
	}
	path, err = store.Write(record)
	if err != nil {
		return err
	}
	shutdown := make(chan error, 1)
	go func() { <-ctx.Done(); shutdown <- server.Shutdown(context.Background()) }()
	err = server.Serve(listener)
	cancel()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, <-shutdown)
}
