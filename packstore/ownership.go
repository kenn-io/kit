package packstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/safefileio"
)

const ownershipMarkerName = ".packstore-owner.json"
const maxOwnershipMarkerBytes = 4096

// Ownership is the fencing term for one application-owned store namespace.
type Ownership struct {
	Format uint32  `json:"format"`
	Vault  string  `json:"vault"`
	Store  StoreID `json:"store"`
	Epoch  string  `json:"epoch"`
}

// Validate checks marker identity independently of any backend binding.
func (o Ownership) Validate() error {
	if o.Format != OwnershipFormatV1 {
		return fmt.Errorf("packstore: unsupported ownership format %d", o.Format)
	}
	if o.Vault == "" {
		return fmt.Errorf("packstore: empty ownership vault")
	}
	if o.Store == "" {
		return fmt.Errorf("packstore: empty ownership store")
	}
	if o.Epoch == "" {
		return fmt.Errorf("packstore: empty ownership epoch")
	}
	if !utf8.ValidString(o.Vault) ||
		!utf8.ValidString(string(o.Store)) ||
		!utf8.ValidString(o.Epoch) {
		return fmt.Errorf("packstore: ownership identity is not valid UTF-8")
	}
	return nil
}

// OwnershipMismatchError reports that the live namespace fencing term differs
// from the binding expected by the caller.
type OwnershipMismatchError struct {
	Expected Ownership
	Actual   Ownership
	Err      error
}

func (e *OwnershipMismatchError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf(
			"%v: expected store %s epoch %q: %v",
			ErrStoreFenced,
			e.Expected.Store,
			e.Expected.Epoch,
			e.Err,
		)
	}
	return fmt.Sprintf(
		"%v: expected store %s epoch %q, found store %s epoch %q",
		ErrStoreFenced,
		e.Expected.Store,
		e.Expected.Epoch,
		e.Actual.Store,
		e.Actual.Epoch,
	)
}

func (e *OwnershipMismatchError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrStoreFenced}
	}
	return []error{ErrStoreFenced, e.Err}
}

type ownershipState struct {
	mu       sync.RWMutex
	expected *Ownership
}

func (s *ownershipState) set(expected Ownership) {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := expected
	s.expected = &copy
}

func (s *ownershipState) get() *Ownership {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.expected == nil {
		return nil
	}
	copy := *s.expected
	return &copy
}

// MarshalOwnership returns the canonical ownership-marker representation.
func MarshalOwnership(value Ownership) ([]byte, error) {
	if err := value.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("packstore: encode ownership marker: %w", err)
	}
	return append(encoded, '\n'), nil
}

// ParseOwnership strictly validates a canonical ownership marker.
func ParseOwnership(data []byte) (Ownership, error) {
	if len(data) > maxOwnershipMarkerBytes {
		return Ownership{}, fmt.Errorf("packstore: ownership marker size %d is invalid", len(data))
	}
	var value Ownership
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Ownership{}, fmt.Errorf("packstore: decode ownership marker: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Ownership{}, fmt.Errorf("packstore: ownership marker contains trailing JSON")
	}
	canonical, err := MarshalOwnership(value)
	if err != nil {
		return Ownership{}, err
	}
	if !bytes.Equal(data, canonical) {
		return Ownership{}, fmt.Errorf("packstore: ownership marker is not canonical")
	}
	return value, nil
}

func readOwnership(path string) (Ownership, error) {
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		return Ownership{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return Ownership{}, fmt.Errorf("packstore: stat ownership marker: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Ownership{}, fmt.Errorf("packstore: ownership marker is not a regular file")
	}
	if info.Size() < 0 || info.Size() > maxOwnershipMarkerBytes {
		return Ownership{}, fmt.Errorf("packstore: ownership marker size %d is invalid", info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(file, maxOwnershipMarkerBytes+1))
	if err != nil {
		return Ownership{}, fmt.Errorf("packstore: read ownership marker: %w", err)
	}
	return ParseOwnership(data)
}

func replaceOwnership(
	ctx context.Context,
	path string,
	next Ownership,
	expected *Ownership,
) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := MarshalOwnership(next)
	if err != nil {
		return err
	}
	current, readErr := readOwnership(path)
	switch {
	case expected == nil && readErr == nil:
		return &OwnershipMismatchError{Actual: current}
	case expected == nil && !errors.Is(readErr, fs.ErrNotExist):
		return readErr
	case expected != nil && readErr != nil:
		return &OwnershipMismatchError{Expected: *expected, Err: readErr}
	case expected != nil && current != *expected:
		return &OwnershipMismatchError{Expected: *expected, Actual: current}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("packstore: prepare ownership directory: %w", err)
	}
	staged, err := os.CreateTemp(filepath.Dir(path), ".ownership-")
	if err != nil {
		return fmt.Errorf("packstore: create ownership staging: %w", err)
	}
	stagedPath := staged.Name()
	stagedOpen := true
	defer func() {
		if stagedOpen {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return fmt.Errorf("packstore: protect ownership staging: %w", err)
	}
	if _, err := staged.Write(encoded); err != nil {
		return fmt.Errorf("packstore: write ownership staging: %w", err)
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("packstore: sync ownership staging: %w", err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("packstore: close ownership staging: %w", err)
	}
	stagedOpen = false
	if err := replaceOwnershipFile(stagedPath, path); err != nil {
		return fmt.Errorf("packstore: publish ownership marker: %w", err)
	}
	if err := pack.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("packstore: sync ownership marker: %w", err)
	}
	return nil
}
