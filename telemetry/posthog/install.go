package posthog

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
)

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
// is replaced with a new install. Processes that share dir get the same
// install: a creator that loses the race reads the winner's file.
func LoadOrCreateInstall(dir string) (Install, error) {
	path := filepath.Join(dir, InstallFileName)
	inst, err := readInstall(path)
	switch {
	case err == nil:
		return inst, nil
	case errors.Is(err, errInvalidInstall):
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Install{}, fmt.Errorf("replace telemetry install file: %w", err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return Install{}, fmt.Errorf("read telemetry install file: %w", err)
	}
	inst = Install{ID: rand.Text(), InstalledAt: time.Now().UTC()}
	data, err := json.Marshal(inst)
	if err != nil {
		return Install{}, fmt.Errorf("encode telemetry install file: %w", err)
	}
	if err := atomicfile.WriteNew(path, data, atomicfile.WithPrivate()); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readInstall(path)
		}
		return Install{}, fmt.Errorf("create telemetry install file: %w", err)
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
