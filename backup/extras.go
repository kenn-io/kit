package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

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
	// escapes the root, and readRegularFile fstats the opened descriptor. This
	// mirrors the attachment capture confinement.
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
	// addFile reads recordPath's bytes through readRoot (confined so a symlink
	// swapped in for a checked regular file cannot escape it) and records the
	// tree entry under recordPath. readRel is recordPath's location relative to
	// readRoot.
	addFile := func(readRoot *os.Root, readRel, recordPath string) error {
		content, info, err := readRegularFile(readRoot, readRel)
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
	if opts.IncludeTokens {
		if err := addDir("tokens"); err != nil {
			return pack.BlobID{}, false, err
		}
		secrets, err := filepath.Glob(filepath.Join(opts.DataDir, "client_secret*.json"))
		if err != nil {
			return pack.BlobID{}, false, fmt.Errorf("backup: globbing client secrets: %w", err)
		}
		for _, s := range secrets {
			rel, err := filepath.Rel(opts.DataDir, s)
			if err != nil {
				return pack.BlobID{}, false, err
			}
			// filepath.Glob doesn't walk a directory tree, so it bypasses
			// addDir's symlink rejection. Lstat (not Stat) reports the link
			// itself for a friendly early error; the confined read through
			// dataRoot below is the authoritative guard against a symlink raced
			// in after this check.
			info, err := os.Lstat(s)
			if err != nil {
				return pack.BlobID{}, false, fmt.Errorf("backup: stat extras file %s: %w", rel, err)
			}
			if !info.Mode().IsRegular() {
				return pack.BlobID{}, false, fmt.Errorf("extras: %s is not a regular file", filepath.ToSlash(rel))
			}
			if err := addFile(dataRoot, rel, rel); err != nil {
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
