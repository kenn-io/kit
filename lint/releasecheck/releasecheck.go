// Package releasecheck validates final release archives against an explicit
// consumer contract. It inspects bytes without extracting or executing them.
package releasecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
)

// Contract lists the complete expected release inventory.
type Contract struct {
	Artifacts []Artifact `json:"artifacts"`
}

// Artifact describes a native release archive and its runtime policy. Baseline
// is a reviewed consumer claim; header/import checks do not prove ABI floors,
// delay imports, dynamically loaded dependencies, or loader search behavior.
type Artifact struct {
	Name            string   `json:"name"`
	OS              string   `json:"os"`
	Arch            string   `json:"arch"`
	Executable      string   `json:"executable"`
	Kind            string   `json:"kind"`
	Baseline        string   `json:"baseline"`
	SystemLibraries []string `json:"system_libraries"`
	RuntimeFiles    []string `json:"runtime_files"`
	RequiredFiles   []string `json:"required_files"`
}

// ReadContract strictly decodes and validates the entire contract, rejecting
// unknown fields, trailing JSON, and ambiguous or inconsistent declarations.
func ReadContract(r io.Reader) (Contract, error) {
	var c Contract
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("decode contract: %w", err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return c, errors.New("contract contains trailing JSON")
		}
		return c, fmt.Errorf("trailing contract data: %w", err)
	}
	if err := validateContract(c, "VERSION"); err != nil {
		return c, err
	}
	return c, nil
}

func safeComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._+-", r) {
			continue
		}
		return false
	}
	return true
}

func memberPath(s string) bool {
	if s == "" || strings.ContainsAny(s, "\\:\x00") || strings.HasPrefix(s, "/") || path.Clean(s) != s || s == "." {
		return false
	}
	for p := range strings.SplitSeq(s, "/") {
		if p == ".." || p == "." || p == "" {
			return false
		}
	}
	return !strings.ContainsFunc(s, unicode.IsControl)
}

// targetMemberPath rejects Win32 aliases as well as slash traversal. Device
// names are reserved in every component, including extension forms. See
// https://learn.microsoft.com/windows/win32/fileio/naming-a-file.
func targetMemberPath(s string, windows bool) bool {
	if !memberPath(s) {
		return false
	}
	if !windows {
		return true
	}
	if strings.ContainsAny(s, "<>\"|?*") {
		return false
	}
	for component := range strings.SplitSeq(s, "/") {
		if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return false
		}
		base, _, _ := strings.Cut(component, ".")
		base = strings.ToUpper(strings.TrimRight(base, " "))
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return false
		}
		for _, prefix := range []string{"COM", "LPT"} {
			if suffix, ok := strings.CutPrefix(base, prefix); ok && len([]rune(suffix)) == 1 && strings.ContainsAny(suffix, "123456789¹²³") {
				return false
			}
		}
	}
	return true
}

func validateContract(c Contract, version string) error {
	if len(c.Artifacts) == 0 {
		return errors.New("contract has no artifacts")
	}
	names, platforms := map[string]bool{}, map[string]bool{}
	for i, a := range c.Artifacts {
		bad := func(msg string) error { return fmt.Errorf("artifact %d (%s): %s", i, a.Name, msg) }
		if a.OS != "linux" && a.OS != "darwin" && a.OS != "windows" {
			return bad("unsupported os")
		}
		if a.Arch != "amd64" && a.Arch != "arm64" {
			return bad("unsupported arch")
		}
		platform := a.OS + "/" + a.Arch
		if platforms[platform] {
			return bad("duplicate platform " + platform)
		}
		platforms[platform] = true
		name := strings.ReplaceAll(a.Name, "{version}", version)
		ext := ".tar.gz"
		if a.OS == "windows" {
			ext = ".zip"
		}
		if !safeComponent(name) || !strings.HasSuffix(name, ext) || !targetMemberPath(name, a.OS == "windows") {
			return bad("name must be an exact safe archive basename ending in " + ext)
		}
		if names[name] {
			return bad("duplicate archive name")
		}
		names[name] = true
		if strings.TrimSpace(a.Baseline) == "" {
			return bad("baseline is required")
		}
		switch a.Kind {
		case "static":
			if a.OS != "linux" {
				return bad("static requires ELF/linux")
			}
			fallthrough
		case "single":
			if len(a.RuntimeFiles) > 0 {
				return bad("static/single cannot declare runtime files")
			}
		case "bundle":
			if len(a.RuntimeFiles) == 0 {
				return bad("bundle must declare runtime files")
			}
		default:
			return bad("kind must be static, single, or bundle")
		}
		seen := map[string]bool{}
		paths := append([]string{a.Executable}, a.RuntimeFiles...)
		paths = append(paths, a.RequiredFiles...)
		for _, p := range paths {
			if !targetMemberPath(p, a.OS == "windows") {
				return bad("invalid member path " + p)
			}
			key := p
			if a.OS == "windows" {
				key = strings.ToLower(key)
			}
			if seen[key] {
				return bad("duplicate member declaration " + p)
			}
			seen[key] = true
		}
		libraries := map[string]bool{}
		for _, lib := range a.SystemLibraries {
			if lib == "" || strings.TrimSpace(lib) != lib || strings.ContainsAny(lib, "*?[]\x00\\") || strings.ContainsFunc(lib, unicode.IsControl) {
				return bad("system libraries must be exact import names")
			}
			if a.OS == "windows" && strings.ContainsAny(lib, "/:") {
				return bad("Windows system libraries must be exact DLL names")
			}
			key := lib
			if a.OS == "windows" {
				key = strings.ToLower(lib)
			}
			if libraries[key] {
				return bad("duplicate system library " + lib)
			}
			libraries[key] = true
		}
	}
	return nil
}

// Check validates the whole contract before checking the selected final archives
// and SHA256SUMS. Empty targetOS requires the complete inventory; an explicit OS
// requires every artifact for that OS and cannot select an empty subset.
func Check(ctx context.Context, c Contract, dir, version, targetOS string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	version = strings.TrimPrefix(version, "v")
	if !safeComponent(version) {
		return errors.New("version must be a nonempty safe basename component")
	}
	if err := validateContract(c, version); err != nil {
		return err
	}
	var selected []Artifact
	for _, a := range c.Artifacts {
		if targetOS == "" || a.OS == targetOS {
			a.Name = strings.ReplaceAll(a.Name, "{version}", version)
			selected = append(selected, a)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("os selector %q selects no declared artifacts", targetOS)
	}
	sums, err := readChecksums(ctx, dir, selected)
	if err != nil {
		return err
	}
	for _, a := range selected {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkArchive(ctx, dir, a, sums[a.Name]); err != nil {
			return fmt.Errorf("%s: %w", a.Name, err)
		}
	}
	return nil
}
