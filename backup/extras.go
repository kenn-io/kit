package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.kenn.io/kit/pack"
)

// ExtrasOptions selects which operational files ride along with a snapshot
// (docs/usage/backup.md, Extras).
type ExtrasOptions struct {
	DataDir               string
	ConfigPath            string
	IncludeConfig         bool
	IncludeTokens         bool
	AllowPlaintextSecrets bool
	Encrypted             bool
}

// ExtrasEntry is one captured file in the extras tree.
type ExtrasEntry struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
	Blob string `json:"blob"`
}

// ExtrasTree is the small JSON tree object referenced by the manifest.
type ExtrasTree struct {
	Entries []ExtrasEntry `json:"entries"`
}

// enterExtrasDir opens one directory component beneath cur as its own root,
// refusing symlinks: the component must lstat as a real directory and the
// opened descriptor must be that same inode, so a symlink present before the
// lstat and one raced in before the open both fail instead of being followed.
func enterExtrasDir(cur *os.Root, comp string) (*os.Root, error) {
	li, err := cur.Lstat(comp)
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
		return nil, fmt.Errorf("path component %q is not a real directory", comp)
	}
	sub, err := cur.OpenRoot(comp)
	if err != nil {
		return nil, err
	}
	held, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	if !os.SameFile(li, held) {
		_ = sub.Close()
		return nil, fmt.Errorf("path component %q changed during capture", comp)
	}
	return sub, nil
}

// readExtrasLeafNoFollow reads one file directly inside dir, refusing a
// symlink at the name: the lstat rejects one present up front, and SameFile
// against the opened descriptor rejects one raced in between lstat and open.
func readExtrasLeafNoFollow(dir *os.Root, name string) ([]byte, os.FileInfo, error) {
	li, err := dir.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !li.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%q is not a regular file", name)
	}
	f, err := dir.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !os.SameFile(li, info) {
		return nil, nil, fmt.Errorf("%q changed during capture", name)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
}

// readExtrasNoFollow reads rel beneath root with symlink resolution refused
// at every path component. Unlike attachment reads — whose bytes are
// hash-verified against the database's recorded content hash, so a raced-in
// symlink yields a loud mismatch — extras have no expected hash: a symlink
// swapped in after the capture walk's lstat check could silently pull other
// in-DataDir files (tokens, client secrets) into the snapshot under a
// deletions path even when their capture is disabled. os.Root only refuses
// symlinks that escape the root, so each directory component is entered
// through its own verified sub-root and the leaf is tied to its lstat by
// descriptor identity; a swap at any component fails closed instead of being
// followed.
func readExtrasNoFollow(root *os.Root, rel string) ([]byte, os.FileInfo, error) {
	rel = filepath.Clean(rel)
	if !filepath.IsLocal(rel) {
		return nil, nil, fmt.Errorf("extras path %q is not local", rel)
	}
	cur := root
	defer func() {
		if cur != root {
			_ = cur.Close()
		}
	}()
	comps := strings.Split(filepath.ToSlash(rel), "/")
	for _, comp := range comps[:len(comps)-1] {
		sub, err := enterExtrasDir(cur, comp)
		if err != nil {
			return nil, nil, err
		}
		if cur != root {
			_ = cur.Close()
		}
		cur = sub
	}
	return readExtrasLeafNoFollow(cur, comps[len(comps)-1])
}

// CaptureExtras stores extras file blobs and the tree object.
func CaptureExtras(opts ExtrasOptions, appender *PackAppender) (pack.BlobID, bool, error) {
	// config.toml carries API keys (server.api_key) verbatim and the tokens
	// directory holds OAuth secrets, so either flag on an unencrypted repo
	// needs the explicit plaintext override. Fail safe by naming the flag(s)
	// that triggered the guard.
	if (opts.IncludeConfig || opts.IncludeTokens) && !opts.Encrypted && !opts.AllowPlaintextSecrets {
		var flag string
		switch {
		case opts.IncludeConfig && opts.IncludeTokens:
			flag = "--include-config/--include-tokens"
		case opts.IncludeConfig:
			flag = "--include-config"
		default:
			flag = "--include-tokens"
		}
		return pack.BlobID{}, false, fmt.Errorf(
			"backup: %s requires an encrypted repository (use --allow-plaintext-secrets to override)", flag)
	}
	// The deletions/tokens walks and the client-secret glob all read files under
	// DataDir; route every read through one root confined to it so a regular
	// file swapped for a symlink between the walk/glob check and the read (a
	// TOCTOU the plain os.ReadFile below used to lose) cannot pull a host file
	// from outside DataDir into the extras blob. os.Root refuses any path that
	// escapes the root, and readExtrasNoFollow additionally refuses symlink
	// resolution at every path component, so a raced-in symlink cannot
	// redirect the read even to a target still inside DataDir — extras bytes
	// carry no expected hash, so unlike attachment reads a redirect would be
	// silent.
	var dataRoot *os.Root
	if opts.DataDir != "" {
		dr, err := os.OpenRoot(opts.DataDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return pack.BlobID{}, false, fmt.Errorf("backup: opening data dir for extras: %w", err)
		}
		if err == nil {
			dataRoot = dr
			defer func() { _ = dataRoot.Close() }()
		}
	}
	var entries []ExtrasEntry
	// addFile reads recordPath's bytes through readRoot with no symlink
	// resolution at any component (readExtrasNoFollow) and records the tree
	// entry under recordPath. readRel is recordPath's location relative to
	// readRoot.
	addFile := func(readRoot *os.Root, readRel, recordPath string) error {
		content, info, err := readExtrasNoFollow(readRoot, readRel)
		if err != nil {
			return fmt.Errorf("backup: reading extras file %s: %w", recordPath, err)
		}
		id, _, err := appender.Add(content)
		if err != nil {
			return err
		}
		entries = append(entries, ExtrasEntry{
			Path: filepath.ToSlash(recordPath),
			Mode: uint32(info.Mode().Perm()),
			Size: int64(len(content)),
			Blob: id.String(),
		})
		return nil
	}
	addDir := func(name string) error {
		if opts.DataDir == "" {
			return nil
		}
		root := filepath.Join(opts.DataDir, name)
		if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("backup: walking %s: %w", name, err)
			}
			if d.IsDir() {
				return nil
			}
			// Reject non-regular files: symlinks, sockets, devices, etc.
			// Symlinks are an attack surface — they could point outside DataDir.
			if !d.Type().IsRegular() {
				if d.Type()&fs.ModeSymlink != 0 {
					rel, err := filepath.Rel(opts.DataDir, path)
					if err != nil {
						rel = path
					}
					return fmt.Errorf("extras: %s is not a regular file", filepath.ToSlash(rel))
				}
				// Skip other non-regular types silently.
				return nil
			}
			rel, err := filepath.Rel(opts.DataDir, path)
			if err != nil {
				return err
			}
			return addFile(dataRoot, rel, rel)
		})
	}
	if err := addDir("deletions"); err != nil {
		return pack.BlobID{}, false, err
	}
	if opts.IncludeConfig && opts.ConfigPath != "" {
		// config.toml may live outside DataDir, so confine the read to its own
		// parent directory: a symlink swapped in at ConfigPath is then refused
		// rather than followed to an arbitrary host file.
		cfgRoot, err := os.OpenRoot(filepath.Dir(opts.ConfigPath))
		if err != nil {
			return pack.BlobID{}, false, fmt.Errorf("backup: opening config dir for extras: %w", err)
		}
		err = addFile(cfgRoot, filepath.Base(opts.ConfigPath), "config.toml")
		_ = cfgRoot.Close()
		if err != nil {
			return pack.BlobID{}, false, err
		}
	}
	// Matching client-secret files only makes sense with a confined root to read
	// through: with no DataDir there would be nothing to scan, and addFile would
	// nil-deref on the absent dataRoot.
	if opts.IncludeTokens && dataRoot != nil {
		if err := addDir("tokens"); err != nil {
			return pack.BlobID{}, false, err
		}
		// Match by basename through the confined root instead of
		// filepath.Glob(filepath.Join(DataDir, ...)): a DataDir path containing
		// glob metacharacters ([, *, ?) would otherwise make Glob silently skip
		// real matches or pull in siblings. The pattern is a pure basename with
		// no directory part, so it never interacts with the data dir path.
		dirEntries, err := fs.ReadDir(dataRoot.FS(), ".")
		if err != nil {
			return pack.BlobID{}, false, fmt.Errorf("backup: reading data dir for client secrets: %w", err)
		}
		for _, e := range dirEntries {
			name := e.Name()
			match, err := filepath.Match("client_secret*.json", name)
			if err != nil {
				return pack.BlobID{}, false, fmt.Errorf("backup: matching client secrets: %w", err)
			}
			if !match {
				continue
			}
			// ReadDir reports the entry's own type (Lstat semantics, not
			// followed), so a symlink or non-regular file named like a client
			// secret yields a friendly early error; the confined read through
			// dataRoot below is the authoritative guard against a symlink raced
			// in after this check.
			if !e.Type().IsRegular() {
				return pack.BlobID{}, false, fmt.Errorf("extras: %s is not a regular file", name)
			}
			if err := addFile(dataRoot, name, name); err != nil {
				return pack.BlobID{}, false, err
			}
		}
	}
	if len(entries) == 0 {
		return pack.BlobID{}, false, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	data, err := json.MarshalIndent(&ExtrasTree{Entries: entries}, "", "  ")
	if err != nil {
		return pack.BlobID{}, false, fmt.Errorf("backup: marshaling extras tree: %w", err)
	}
	id, _, err := appender.Add(data)
	if err != nil {
		return pack.BlobID{}, false, err
	}
	return id, true, nil
}
