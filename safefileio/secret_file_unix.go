//go:build unix

package safefileio

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenSecretFile opens a regular secret file through a trusted Unix path.
// Every directory and symlink hop must be controlled by root or the effective
// user. Current-user files require 0400/0600; root files deny group/other write.
// Platform ACL/filesystem checks apply; permissions are never repaired.
func OpenSecretFile(path string) (*os.File, error) {
	resolved, expected, err := trustedSecretPath(path)
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(resolved, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !os.SameFile(expected, info) {
		err = errors.New("secret file changed during opening")
	}
	if err == nil {
		err = validateSecretInfo(info, uint32(os.Geteuid()))
	}
	if err == nil {
		err = validateSecretAccess(file)
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validateSecretInfo(info fs.FileInfo, uid uint32) error {
	if info == nil {
		return errors.New("secret metadata unavailable")
	}
	if !info.Mode().IsRegular() {
		return errors.New("secret must be a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("secret owner unavailable")
	}
	mode := info.Mode().Perm()
	if stat.Uid == 0 && mode&0o022 == 0 {
		return nil
	}
	if stat.Uid == uid && (mode == 0o400 || mode == 0o600) {
		return nil
	}
	return errors.New("secret must be private (0400/0600) or root-owned without group/other write")
}

func trustedSecretPath(path string) (string, fs.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return "", nil, errors.New("secret path must be absolute")
	}
	pending := strings.Split(strings.TrimPrefix(path, "/"), "/")
	current := "/"
	links := 0
	for len(pending) > 0 {
		part := pending[0]
		pending = pending[1:]
		// Preserve dot components so the kernel enforces search permissions on
		// every directory the configured path actually traverses.
		next := current
		if !strings.HasSuffix(current, string(filepath.Separator)) {
			next += string(filepath.Separator)
		}
		next += part
		child, err := os.Lstat(next)
		if err != nil {
			return "", nil, err
		}
		dir, err := openSecretDirectory(current)
		if err != nil {
			return "", nil, err
		}
		info, err := dir.Stat()
		if err == nil {
			err = requireUnsharedParentForUID(info, child, uint32(os.Geteuid()))
		}
		if err == nil {
			err = validateSecretParentAccess(dir)
		}
		_ = dir.Close()
		if err != nil {
			return "", nil, err
		}
		if child.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", nil, errors.New("too many secret symlink hops")
			}
			stat, ok := child.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
				return "", nil, errors.New("secret symlink has an untrusted owner")
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", nil, err
			}
			if filepath.IsAbs(target) {
				current = "/"
				target = strings.TrimPrefix(target, "/")
			}
			pending = append(strings.Split(target, "/"), pending...)
			continue
		}
		current = next
	}
	info, err := os.Lstat(current)
	if err != nil {
		return "", nil, fmt.Errorf("stat secret: %w", err)
	}
	return current, info, nil
}
