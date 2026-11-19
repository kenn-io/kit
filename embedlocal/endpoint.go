package embedlocal

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
)

// Service identifies the shared embeddings server and its runtime records.
const Service = "kit-embed"

// Version identifies the server runtime protocol.
const Version = "1"

// TextModel and FaceModel select installed model assets and server capabilities.
const (
	TextModel = embedassets.TextModel
	FaceModel = embedassets.FaceModel
)

// RestartRequiredError identifies a running server predating face support.
type RestartRequiredError struct{ PID int }

func (e *RestartRequiredError) Error() string {
	return fmt.Sprintf("restart kennembed PID %d to enable face embeddings", e.PID)
}

// ErrNotFound means no local server was found and no executable was supplied.
var ErrNotFound = errors.New("local embeddings server not found")

// Options configures discovery and optional startup. DataDir defaults to
// DefaultDir. Executable is the installed kennembed binary; leaving it empty
// permits discovery without starting a server.
type Options struct {
	DataDir, Executable string
	// Model selects TextModel (the default) or FaceModel.
	Model string
	// SkipOllama skips Ollama and discovers kennembed, starting it only when
	// Executable is supplied.
	SkipOllama bool
}

// DefaultDir returns the shared per-user embeddings data directory.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kenn", "embeddings"), nil
}

// Endpoint returns a /v1 base URL, preferring a running Ollama to kennembed
// unless SkipOllama is set.
// With SkipOllama set, an empty Executable and no running kennembed return
// ErrNotFound.
// Startup follows ctx's deadline, or waits at most two minutes without one.
// A started server continues loading after ctx is canceled.
func Endpoint(ctx context.Context, opts Options) (string, error) {
	return endpoint(ctx, opts, "http://127.0.0.1:11434", 2*time.Minute)
}

func endpoint(ctx context.Context, opts Options, ollama string, timeout time.Duration) (string, error) {
	if _, _, err := embedassets.SelectedModel(opts.DataDir, opts.Model); err != nil {
		return "", err
	}
	if opts.Model == "" {
		opts.Model = TextModel
	}
	if opts.Model == TextModel && !opts.SkipOllama && probeOllama(ctx, ollama) {
		return ollama + "/v1", nil
	}
	var err error
	if opts.DataDir == "" {
		opts.DataDir, err = DefaultDir()
		if err != nil {
			return "", err
		}
	}
	opts.DataDir, err = filepath.Abs(opts.DataDir)
	if err != nil {
		return "", err
	}
	manager := daemon.Manager{
		Store: daemon.RuntimeStore{Dir: opts.DataDir, Prefix: Service},
		Discover: daemon.DiscoverOptions{
			RequirePIDAlive: true,
			Probe:           daemon.ProbeOptions{ExpectedService: Service},
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == Version
		},
	}
	if opts.Executable == "" {
		rec, _, ok, err := manager.Find(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", ErrNotFound
		}
		return modelEndpoint(ctx, rec, opts.Model)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	path := filepath.Join(opts.DataDir, "kennembed.log")
	discovery := manager
	joining, current := false, false
	exited := make(chan error, 1)
	manager.FindFunc = func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
		rec, info, ok, err := discovery.Find(ctx)
		if err == nil && !ok {
			held, probeErr := ownerHeld(ctx, manager.Store)
			if probeErr != nil {
				err = probeErr
			} else if held {
				joining, current = true, true
			} else if err = startupFailure(joining, exited); err != nil {
				cancel(err)
			}
		}
		return rec, info, ok, err
	}
	manager.Start = func(ctx context.Context) error {
		if joining {
			return nil
		}
		log, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600)
		if current = err == nil; !current {
			return fmt.Errorf("open embeddings log: %w", err)
		}
		err = daemon.StartDetached(ctx, daemon.StartDetachedOptions{
			Executable: opts.Executable,
			Args:       []string{"-data-dir", opts.DataDir, "-listen", "127.0.0.1:0"},
			Stderr:     log,
			Exited:     func(err error) { exited <- err },
		})
		return errors.Join(err, log.Close())
	}
	if deadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(deadline)
	}
	rec, _, err := manager.Ensure(ctx, timeout)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("waiting for embeddings server readiness: %w", err)
		}
		return "", startupError(path, err, current)
	}
	return modelEndpoint(ctx, rec, opts.Model)
}

func probeOllama(ctx context.Context, origin string) bool {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/show", strings.NewReader(`{"model":"nomic-embed-text"}`))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func ownerHeld(ctx context.Context, store daemon.RuntimeStore) (bool, error) {
	probe, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	release, err := store.AcquireOwnerLock(probe)
	if err == nil {
		release()
		return false, nil
	}
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return true, nil
	}
	return false, err
}

func startupFailure(joining bool, exited <-chan error) error {
	select {
	case err := <-exited:
		if err == nil {
			err = errors.New("server exited before readiness")
		}
		return fmt.Errorf("embeddings server exited: %w", err)
	default:
	}
	if joining {
		return errors.New("embeddings startup owner released before readiness")
	}
	return nil
}

func startupError(path string, cause error, current bool) error {
	failure := fmt.Errorf("%w; log %s", cause, path)
	if !current {
		return failure
	}
	log, err := os.Open(path)
	if err != nil {
		return failure
	}
	defer log.Close()
	info, err := log.Stat()
	if err != nil {
		return errors.Join(failure, err)
	}
	tail, err := io.ReadAll(io.NewSectionReader(log, max(0, info.Size()-2048), 2048))
	return errors.Join(fmt.Errorf("%w:\n%s", failure, strings.TrimSpace(string(tail))), err)
}

// Old text servers retain their existing contract; faces require capability discovery.
func modelEndpoint(ctx context.Context, rec daemon.RuntimeRecord, model string) (string, error) {
	base := rec.Endpoint().BaseURL() + "/v1"
	ctx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return "", err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if model == TextModel {
			return base, nil
		}
		return "", &RestartRequiredError{PID: rec.PID}
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("model discovery: HTTP %d", resp.StatusCode)
	}
	var available struct {
		Models []string `json:"models"`
	}
	if err := json.UnmarshalRead(io.LimitReader(resp.Body, 4096), &available); err != nil {
		return "", fmt.Errorf("model discovery: %w", err)
	}
	if !slices.Contains(available.Models, model) {
		return "", ErrNotFound
	}
	return base, nil
}
