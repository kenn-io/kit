package posthog

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"go.kenn.io/kit/atomicfile"
)

type installToken struct {
	mu                                         sync.Mutex
	path, distinctID, application, registerURL string
	logger                                     *slog.Logger
	cached                                     string
}

type storedInstallToken struct {
	DistinctID  string `json:"distinct_id"`
	Application string `json:"application"`
	Token       string `json:"token,omitempty"`
}

func (t *installToken) get(ctx context.Context, base http.RoundTripper) (string, *http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != "" {
		return t.cached, nil, nil
	}
	lock := flock.New(t.path + ".lock")
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return "", nil, fmt.Errorf("lock telemetry token: %w", err)
	}
	if !locked {
		return "", nil, errors.New("lock telemetry token: lock not acquired")
	}
	defer func() { _ = lock.Unlock() }()
	data, err := os.ReadFile(t.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", nil, fmt.Errorf("read telemetry token: %w", err)
	}
	var stored storedInstallToken
	if err == nil && json.Unmarshal(data, &stored) == nil && stored.DistinctID == t.distinctID && stored.Application == t.application && strings.TrimSpace(stored.Token) != "" {
		t.cached = stored.Token
		return t.cached, nil, nil
	}
	stored = storedInstallToken{DistinctID: t.distinctID, Application: t.application}
	data, err = json.Marshal(stored)
	if err != nil {
		return "", nil, fmt.Errorf("encode telemetry registration: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.registerURL, bytes.NewReader(data))
	if err != nil {
		return "", nil, fmt.Errorf("create telemetry registration: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := base.RoundTrip(req)
	if err != nil {
		return "", nil, fmt.Errorf("register telemetry install: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", response, nil
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, 4*1024))
	if err != nil {
		return "", nil, fmt.Errorf("read telemetry registration: %w", err)
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", nil, fmt.Errorf("decode telemetry registration: %w", err)
	}
	if strings.TrimSpace(result.Token) == "" {
		return "", nil, errors.New("telemetry registration returned an empty token")
	}
	stored.Token = result.Token
	data, err = json.Marshal(stored)
	if err != nil {
		return "", nil, fmt.Errorf("encode telemetry token: %w", err)
	}
	t.cached = result.Token
	if err := atomicfile.WriteFile(t.path, data, atomicfile.WithPrivate()); err != nil {
		t.logger.WarnContext(ctx, "write telemetry token failed", "error", err)
	}
	return t.cached, nil, nil
}

// InstallFileName is the file LoadOrCreateInstall keeps in its directory.
const InstallFileName = "telemetry-install.json"

var errInvalidInstall = errors.New("invalid telemetry install file")

// Install is an anonymous installation identity for Options.DistinctID and
// Options.InstalledAt.
type Install struct {
	ID          string    `json:"install_id"`
	InstalledAt time.Time `json:"installed_at"`
}

// LoadOrCreateInstall returns the install stored in dir, creating
// InstallFileName as a private file on first use. A file that does not parse
// is replaced with a new install. Every call holds a lock file beside it, so
// processes that share dir get the same install.
func LoadOrCreateInstall(dir string) (Install, error) {
	path := filepath.Join(dir, InstallFileName)
	// Reads take the lock too: Windows can't replace a file another caller has open.
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		return Install{}, fmt.Errorf("lock telemetry install file: %w", err)
	}
	defer func() { _ = lock.Unlock() }()
	inst, err := readInstall(path)
	if err == nil {
		return inst, nil
	}
	if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, errInvalidInstall) {
		return Install{}, fmt.Errorf("read telemetry install file: %w", err)
	}
	inst = Install{ID: rand.Text(), InstalledAt: time.Now().UTC()}
	data, err := json.Marshal(inst)
	if err != nil {
		return Install{}, fmt.Errorf("encode telemetry install file: %w", err)
	}
	if err := atomicfile.WriteFile(path, data, atomicfile.WithPrivate()); err != nil {
		return Install{}, fmt.Errorf("write telemetry install file: %w", err)
	}
	return inst, nil
}

func readInstall(path string) (Install, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Install{}, err
	}
	var inst Install
	if err := json.Unmarshal(data, &inst); err != nil || strings.TrimSpace(inst.ID) == "" {
		return Install{}, fmt.Errorf("%w %s", errInvalidInstall, path)
	}
	return inst, nil
}
