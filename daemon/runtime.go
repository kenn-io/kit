package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RuntimeRecord is the on-disk daemon.<pid>.json shape used for discovery.
type RuntimeRecord struct {
	PID       int               `json:"pid"`
	Network   string            `json:"network,omitempty"`
	Address   string            `json:"address"`
	Service   string            `json:"service,omitempty"`
	Version   string            `json:"version,omitempty"`
	StartedAt time.Time         `json:"started_at,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`

	// SourcePath is set by List and ignored on disk.
	SourcePath string `json:"-"`
}

// NewRuntimeRecord returns a record for the current process.
func NewRuntimeRecord(service, version string, ep Endpoint) RuntimeRecord {
	return RuntimeRecord{
		PID:       os.Getpid(),
		Network:   ep.Network,
		Address:   ep.Address,
		Service:   service,
		Version:   version,
		StartedAt: time.Now().UTC(),
	}
}

// Endpoint returns the daemon endpoint described by r.
func (r RuntimeRecord) Endpoint() Endpoint {
	network := r.Network
	address := r.Address
	if path, ok := strings.CutPrefix(address, "unix://"); ok {
		network = NetworkUnix
		address = path
	}
	if network == "" {
		network = NetworkTCP
	}
	return Endpoint{Network: network, Address: address}
}

// RuntimeStore owns daemon runtime files in a directory.
type RuntimeStore struct {
	Dir    string
	Prefix string
}

func (s RuntimeStore) prefix() string {
	if s.Prefix == "" {
		return "daemon"
	}
	return s.Prefix
}

func (s RuntimeStore) validatePrefix() (string, error) {
	prefix := s.prefix()
	if prefix == "." || prefix == ".." ||
		strings.ContainsAny(prefix, `/\`) ||
		filepath.Base(prefix) != prefix {
		return "", fmt.Errorf("runtime prefix %q must be a basename", prefix)
	}
	return prefix, nil
}

func (s RuntimeStore) prepareDir() error {
	if s.Dir == "" {
		return fmt.Errorf("runtime dir is empty")
	}
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("runtime dir %q must be absolute", s.Dir)
	}
	if err := ensurePrivateRuntimeDir(s.Dir); err != nil {
		return fmt.Errorf("prepare runtime dir: %w", err)
	}
	return nil
}

// Path returns the runtime file path for pid.
func (s RuntimeStore) Path(pid int) (string, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	if pid <= 0 {
		return "", fmt.Errorf("pid must be > 0")
	}
	return filepath.Join(s.Dir, fmt.Sprintf("%s.%d.json", prefix, pid)), nil
}

// LockPath returns the path used to serialize daemon auto-starts for the store.
func (s RuntimeStore) LockPath() (string, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	if err := s.prepareDir(); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, prefix+".lock"), nil
}

// ListenLockPath returns the path used to serialize daemon server listen setup
// for the store.
func (s RuntimeStore) ListenLockPath() (string, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	if err := s.prepareDir(); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, prefix+".listen.lock"), nil
}

// Write saves rec atomically and returns the final path.
func (s RuntimeStore) Write(rec RuntimeRecord) (string, error) {
	if err := s.prepareDir(); err != nil {
		return "", err
	}
	if rec.PID <= 0 {
		return "", fmt.Errorf("pid must be > 0")
	}
	if rec.Address == "" {
		return "", fmt.Errorf("runtime address is empty")
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now().UTC()
	}
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	final, err := s.Path(rec.PID)
	if err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal runtime record: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, fmt.Sprintf("%s.%d.*.json.tmp", prefix, rec.PID))
	if err != nil {
		return "", fmt.Errorf("create runtime temp file: %w", err)
	}
	tmpPath := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write runtime temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("chmod runtime temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close runtime temp file: %w", err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return "", fmt.Errorf("rename runtime file: %w", err)
	}
	success = true
	return final, nil
}

// Read parses one runtime file.
func (s RuntimeStore) Read(path string) (RuntimeRecord, error) {
	if err := s.prepareDir(); err != nil {
		return RuntimeRecord{}, err
	}
	return s.readPrepared(path)
}

func (s RuntimeStore) readPrepared(path string) (RuntimeRecord, error) {
	file, err := openRuntimeFile(path)
	if err != nil {
		return RuntimeRecord{}, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(file)
	if err != nil {
		return RuntimeRecord{}, fmt.Errorf("read runtime file %s: %w", path, err)
	}
	var rec RuntimeRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return RuntimeRecord{}, fmt.Errorf("parse runtime file %s: %w", path, err)
	}
	rec.SourcePath = path
	return rec, nil
}

// List returns valid runtime records in deterministic order. Malformed files
// and temp files are ignored.
func (s RuntimeStore) List() ([]RuntimeRecord, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return nil, err
	}
	if err := s.prepareDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runtime dir: %w", err)
	}
	var records []RuntimeRecord
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		filenamePID, ok := pidFromName(prefix, name)
		if !ok {
			continue
		}
		path := filepath.Join(s.Dir, name)
		rec, err := s.readPrepared(path)
		if err != nil || rec.PID != filenamePID || rec.Address == "" {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].PID < records[j].PID
		}
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}

// CleanupDead removes runtime files whose filename PID matches the record PID
// and the process is no longer alive. Malformed and PID-mismatched files are
// left in place for human inspection.
func (s RuntimeStore) CleanupDead() (int, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return 0, err
	}
	if err := s.prepareDir(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read runtime dir: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		filenamePID, ok := pidFromName(prefix, name)
		if !ok {
			continue
		}
		path := filepath.Join(s.Dir, name)
		rec, err := s.readPrepared(path)
		if err != nil || rec.PID != filenamePID || ProcessAlive(filenamePID) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}

func pidFromName(prefix, name string) (int, bool) {
	filePrefix := prefix + "."
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), ".json")
	pid, err := strconv.Atoi(mid)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
