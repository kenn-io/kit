package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"go.kenn.io/kit/atomicfile"
	gitenv "go.kenn.io/kit/git/env"
)

type frontmatterExcludes []string

func (e *frontmatterExcludes) String() string { return strings.Join(*e, ", ") }

func (e *frontmatterExcludes) Set(value string) error {
	for part := range strings.SplitSeq(value, "/") {
		if part != "**" {
			if _, err := path.Match(part, ""); err != nil {
				return fmt.Errorf("invalid exclude %q: %w", value, err)
			}
		}
	}
	*e = append(*e, value)
	return nil
}

// frontmatterGlob matches slash-separated paths; ** matches zero or more
// complete path components. Other components use path.Match syntax.
func frontmatterGlob(pattern, name string) bool {
	p, n := strings.Split(pattern, "/"), strings.Split(name, "/")
	var match func(int, int) bool
	match = func(i, j int) bool {
		if i == len(p) {
			return j == len(n)
		}
		if p[i] == "**" {
			return match(i+1, j) || (j < len(n) && match(i, j+1))
		}
		if j == len(n) {
			return false
		}
		ok, _ := path.Match(p[i], n[j]) // Patterns were validated by Set.
		return ok && match(i+1, j+1)
	}
	return match(0, 0)
}

func frontmatterGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitenv.StripInherited(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type frontmatterBlock struct {
	doc     yaml.Node
	fields  map[string]*yaml.Node
	body    []byte
	newline string
}

var errFrontmatterMissing = errors.New("missing opening --- frontmatter delimiter")

func parseFrontmatter(src []byte) (*frontmatterBlock, error) {
	lines := bytes.SplitAfter(src, []byte("\n"))
	lineText := func(line []byte) string { return strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r") }
	if lineText(lines[0]) != "---" {
		return nil, errFrontmatterMissing
	}
	end := 1
	for end < len(lines) && lineText(lines[end]) != "---" {
		end++
	}
	if end == len(lines) {
		return nil, errors.New("missing closing --- frontmatter delimiter")
	}
	b := &frontmatterBlock{body: bytes.Join(lines[end+1:], nil), newline: "\n"}
	if bytes.HasSuffix(lines[0], []byte("\r\n")) {
		b.newline = "\r\n"
	}
	header := bytes.Join(lines[1:end], nil)
	dec := yaml.NewDecoder(bytes.NewReader(header))
	if err := dec.Decode(&b.doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid frontmatter YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("frontmatter must contain a single YAML mapping")
	}
	if len(b.doc.Content) != 1 || b.doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("frontmatter must be a YAML mapping")
	}
	// Decode as nodes to reject duplicate keys without coercing dates to time.Time.
	var fields map[string]yaml.Node
	if err := b.doc.Decode(&fields); err != nil {
		return nil, fmt.Errorf("invalid frontmatter YAML: %w", err)
	}
	b.fields = make(map[string]*yaml.Node)
	content := b.doc.Content[0].Content
	for i := 0; i < len(content); i += 2 {
		b.fields[content[i].Value] = content[i+1]
	}
	return b, nil
}

var portableSkillFields = map[string]struct{}{
	"name": {}, "description": {}, "license": {}, "compatibility": {}, "metadata": {}, "allowed-tools": {},
}

// skillFrontmatterProblems enforces the portable Agent Skills profile. In
// particular, Claude Code's local defaults for omitted name and description
// do not satisfy the portable format.
func skillFrontmatterProblems(b *frontmatterBlock, file string) []string {
	var problems []string
	skillDir := path.Dir(file)
	if skillDir == "." {
		problems = append(problems, "SKILL.md must be inside a named skill directory")
	}
	for i := 0; i+1 < len(b.doc.Content[0].Content); i += 2 {
		key := b.doc.Content[0].Content[i]
		if _, ok := portableSkillFields[key.Value]; !ok {
			problems = append(problems, fmt.Sprintf("field %q is not part of portable Agent Skills frontmatter; put vendor-specific data under metadata", key.Value))
		}
	}

	nameNode := b.fields["name"]
	if nameNode == nil {
		problems = append(problems, "name must be a non-empty string")
	} else if nameNode.Kind != yaml.ScalarNode || nameNode.Tag != "!!str" || strings.TrimSpace(nameNode.Value) == "" {
		problems = append(problems, "name must be a non-empty string")
	} else {
		name := nameNode.Value
		if !validSkillName(name) {
			problems = append(problems, "name must be 1-64 lowercase letters, digits, or hyphens, with no leading, trailing, or consecutive hyphens")
		} else if skillDir != "." && name != path.Base(skillDir) {
			problems = append(problems, fmt.Sprintf("name must match skill directory %q", path.Base(skillDir)))
		}
	}

	description := b.fields["description"]
	if description == nil {
		problems = append(problems, "description must be a non-empty string")
	} else if description.Kind != yaml.ScalarNode || description.Tag != "!!str" || strings.TrimSpace(description.Value) == "" {
		problems = append(problems, "description must be a non-empty string")
	} else if length := utf8.RuneCountInString(description.Value); length > 1024 {
		problems = append(problems, "description must be at most 1024 characters")
	}

	if license := b.fields["license"]; license != nil && (!skillString(license) || strings.TrimSpace(license.Value) == "") {
		problems = append(problems, "license must be a non-empty string")
	}
	if compatibility := b.fields["compatibility"]; compatibility != nil {
		if !skillString(compatibility) || strings.TrimSpace(compatibility.Value) == "" {
			problems = append(problems, "compatibility must be a non-empty string")
		} else if length := utf8.RuneCountInString(compatibility.Value); length > 500 {
			problems = append(problems, "compatibility must be at most 500 characters")
		}
	}
	if metadata := b.fields["metadata"]; metadata != nil {
		if metadata.Kind != yaml.MappingNode {
			problems = append(problems, "metadata must be a mapping of string keys to string values")
		} else {
			for i := 0; i+1 < len(metadata.Content); i += 2 {
				key, value := metadata.Content[i], metadata.Content[i+1]
				if !skillString(key) {
					problems = append(problems, "metadata keys must be strings")
				}
				if !skillString(value) {
					problems = append(problems, "metadata values must be strings")
				}
			}
		}
	}
	if allowedTools := b.fields["allowed-tools"]; allowedTools != nil {
		if !skillString(allowedTools) {
			problems = append(problems, "allowed-tools must be a space-separated string")
		}
	}
	return problems
}

func skillString(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str"
}

func validSkillName(name string) bool {
	if len(name) == 0 || len(name) > 64 || name[0] == '-' || name[len(name)-1] == '-' || strings.Contains(name, "--") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	return true
}

func checkSkillFrontmatterFileWithCaseSemantics(root, file string, indexModes map[string]string, symlinkPlaceholders, caseInsensitiveFilesystem bool) ([]string, error) {
	full := filepath.Join(root, filepath.FromSlash(file))
	if _, err := os.Lstat(full); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	resolved, err := frontmatterWorkingPathWithCaseSemantics(root, file, indexModes, symlinkPlaceholders, caseInsensitiveFilesystem)
	if errors.Is(err, errFrontmatterSymlinkOutside) {
		return []string{"Markdown symlink target is outside the repository"}, nil
	}
	if err != nil {
		return nil, err
	}
	src, err := os.ReadFile(resolved)
	if err != nil {
		return nil, err
	}
	b, err := parseFrontmatter(src)
	if err != nil {
		return []string{err.Error()}, nil //nolint:nilerr // Report validation findings and continue scanning other files.
	}
	return skillFrontmatterProblems(b, file), nil
}

func frontmatterSkillDirectories(tracked []byte) map[string]struct{} {
	dirs := make(map[string]struct{})
	for name := range bytes.SplitSeq(bytes.TrimSuffix(tracked, []byte{0}), []byte{0}) {
		file := string(name)
		if path.Base(file) == "SKILL.md" {
			if dir := path.Dir(file); dir != "." {
				dirs[dir] = struct{}{}
			}
		}
	}
	return dirs
}

func frontmatterInSkillDirectory(file string, skillDirs map[string]struct{}) bool {
	for dir := path.Dir(file); ; dir = path.Dir(dir) {
		if _, ok := skillDirs[dir]; ok {
			return true
		}
		if dir == "." {
			return false
		}
	}
}

func frontmatterDate(n *yaml.Node) bool {
	if n == nil || n.Kind != yaml.ScalarNode || len(n.Value) != len(time.DateOnly) {
		return false
	}
	date, err := time.Parse(time.DateOnly, n.Value)
	return err == nil && date.Year() > 0
}

func (b *frontmatterBlock) problems() []string {
	var problems []string
	for _, key := range []string{"title", "description"} {
		n := b.fields[key]
		if n == nil || n.Kind != yaml.ScalarNode || n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" {
			problems = append(problems, key+" must be a non-empty string")
		}
	}
	n := b.fields["last_edited"]
	if n == nil || !frontmatterDate(n) || n.Tag != "!!timestamp" || n.Style != 0 {
		problems = append(problems, "last_edited must be a bare YYYY-MM-DD date")
	}
	return problems
}

func (b *frontmatterBlock) fix(date string) ([]byte, bool, error) {
	n := b.fields["last_edited"]
	switch {
	case n == nil:
		n = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: date}
		b.doc.Content[0].Content = append(b.doc.Content[0].Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "last_edited"}, n)
		b.fields["last_edited"] = n
	case frontmatterDate(n) && (n.Style == yaml.SingleQuotedStyle || n.Style == yaml.DoubleQuotedStyle):
		n.Tag, n.Style = "!!timestamp", 0
	default:
		return nil, false, nil
	}
	var header bytes.Buffer
	enc := yaml.NewEncoder(&header)
	enc.SetIndent(2)
	if err := enc.Encode(&b.doc); err != nil {
		return nil, false, fmt.Errorf("encoding frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, false, err
	}
	result := []byte("---" + b.newline + strings.ReplaceAll(header.String(), "\n", b.newline) + "---" + b.newline)
	return append(result, b.body...), true, nil
}

type frontmatterTreeEntry struct {
	mode    string
	hash    string
	changed bool
}

func frontmatterTree(ctx context.Context, root, base string) (map[string]frontmatterTreeEntry, error) {
	data, err := frontmatterGit(ctx, root, "ls-tree", "-rz", "--full-tree", base)
	if err != nil {
		return nil, err
	}
	tree := make(map[string]frontmatterTreeEntry)
	for record := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		metadata, name, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(metadata)
		if ok && len(fields) == 3 {
			tree[name] = frontmatterTreeEntry{mode: fields[0], hash: fields[2]}
		}
	}
	// Let Git apply its attributes and line-ending rules when deciding whether
	// content changed. Raw blob comparisons would flag every CRLF checkout.
	diff, err := frontmatterGit(ctx, root, "diff", "--name-only", "-z", base, "--")
	if err != nil {
		return nil, err
	}
	for name := range bytes.SplitSeq(diff, []byte{0}) {
		if entry, ok := tree[string(name)]; ok {
			entry.changed = true
			tree[string(name)] = entry
		}
	}
	return tree, nil
}

func frontmatterIndexModes(ctx context.Context, root string) (map[string]string, error) {
	data, err := frontmatterGit(ctx, root, "ls-files", "--stage", "-z", "--cached", "--deduplicate")
	if err != nil {
		return nil, err
	}
	modes := make(map[string]string)
	for record := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		metadata, name, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(metadata)
		if ok && len(fields) == 3 && fields[2] == "0" {
			modes[name] = fields[0]
		}
	}
	return modes, nil
}

func frontmatterSymlinkPlaceholders(ctx context.Context, root string) (bool, error) {
	// A regular file with symlink index mode is a placeholder only when Git
	// materializes symlinks as files; with core.symlinks=true it can be a real
	// working-tree replacement.
	value, err := frontmatterGit(ctx, root, "config", "--bool", "--get", "--default=true", "core.symlinks")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(value)) == "false", nil
}

func frontmatterCaseInsensitiveFilesystem(ctx context.Context, root string) (bool, error) {
	value, err := frontmatterGit(ctx, root, "config", "--bool", "--get", "--default=false", "core.ignoreCase")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(value)) == "true", nil
}

var errFrontmatterSymlinkOutside = errors.New("markdown symlink target is outside the repository")

func frontmatterRepositoryTarget(root, target string) (string, bool, error) {
	windows := filepath.Separator == '\\'
	if !frontmatterIsAbsoluteTarget(target, windows) {
		return filepath.ToSlash(target), false, nil
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", false, err
	}
	if windows && filepath.VolumeName(target) == "" && frontmatterWindowsRootedWithoutVolume(target) {
		target = frontmatterWindowsRootedTarget(filepath.VolumeName(rootAbs), target)
	}
	rootPath := filepath.ToSlash(filepath.Clean(rootAbs))
	absolute := filepath.ToSlash(target)
	if rootPath == "/" {
		relative := strings.TrimPrefix(absolute, "/")
		if relative == "" {
			relative = "."
		}
		return relative, true, nil
	}
	rootParts, targetParts := strings.Split(rootPath, "/"), strings.Split(absolute, "/")
	if len(targetParts) < len(rootParts) {
		return "", false, errFrontmatterSymlinkOutside
	}
	for i, part := range rootParts {
		equal := targetParts[i] == part
		if filepath.Separator == '\\' {
			equal = strings.EqualFold(targetParts[i], part)
		}
		if !equal {
			return "", false, errFrontmatterSymlinkOutside
		}
	}
	relative := strings.Join(targetParts[len(rootParts):], "/")
	if relative == "" {
		relative = "."
	}
	return relative, true, nil
}

func frontmatterIsAbsoluteTarget(target string, windows bool) bool {
	return filepath.IsAbs(target) || path.IsAbs(target) || (windows && filepath.VolumeName(target) == "" && frontmatterWindowsRootedWithoutVolume(target))
}

func frontmatterWindowsRootedWithoutVolume(target string) bool {
	slashed := strings.ReplaceAll(target, `\`, "/")
	return strings.HasPrefix(slashed, "/") && !strings.HasPrefix(slashed, "//")
}

// A Windows path rooted at the current drive's root has no volume of its own.
// Anchor that root to the repository volume before checking containment.
func frontmatterWindowsRootedTarget(rootVolume, target string) string {
	if rootVolume == "" || !frontmatterWindowsRootedWithoutVolume(target) {
		return target
	}
	slashed := strings.ReplaceAll(target, `\`, "/")
	return rootVolume + slashed
}

func frontmatterIndexMode(indexModes map[string]string, name string, caseInsensitiveFilesystem bool) string {
	if mode, ok := indexModes[name]; ok {
		return mode
	}
	if caseInsensitiveFilesystem {
		for indexedName, mode := range indexModes {
			if strings.EqualFold(indexedName, name) {
				return mode
			}
		}
	}
	return ""
}

// Resolve the working path component-by-component so symlinks and .. retain
// filesystem order. Git records symlink targets as regular files when
// core.symlinks=false, so use index modes to identify those placeholders too.
func frontmatterWorkingPathWithCaseSemantics(root, file string, indexModes map[string]string, symlinkPlaceholders, caseInsensitiveFilesystem bool) (string, error) {
	var resolved []string
	pending := strings.Split(filepath.ToSlash(file), "/")
	links := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return "", errFrontmatterSymlinkOutside
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}

		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		full := filepath.Join(root, filepath.FromSlash(candidate))
		info, err := os.Lstat(full)
		if err != nil {
			return "", err
		}
		isSymlink := info.Mode()&os.ModeSymlink != 0
		isPlaceholder := symlinkPlaceholders && !isSymlink && frontmatterIndexMode(indexModes, candidate, caseInsensitiveFilesystem) == "120000"
		if isSymlink || isPlaceholder {
			links++
			if links > 40 {
				return "", errors.New("too many working-tree symlinks")
			}
			var target string
			if isSymlink {
				target, err = os.Readlink(full)
			} else {
				var data []byte
				data, err = os.ReadFile(full)
				target = string(data)
			}
			if err != nil {
				return "", err
			}
			target, absolute, err := frontmatterRepositoryTarget(root, target)
			if err != nil {
				return "", err
			}
			if absolute {
				resolved = nil
			} else {
				resolved = candidateParts[:len(candidateParts)-1]
			}
			pending = append(strings.Split(target, "/"), pending...)
			continue
		}
		if len(pending) > 0 && !info.IsDir() {
			return "", fmt.Errorf("working path component %s is not a directory", candidate)
		}
		resolved = candidateParts
	}
	if len(resolved) == 0 {
		return "", errors.New("markdown path resolves to the repository directory")
	}
	full := filepath.Join(root, filepath.FromSlash(strings.Join(resolved, "/")))
	full, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", err
	}
	if _, err := frontmatterPaths(root, []string{full}); err != nil {
		return "", errFrontmatterSymlinkOutside
	}
	return full, nil
}

// Resolve historical paths component-by-component so intermediate symlinks
// and following .. components match filesystem order.
func frontmatterPrevious(ctx context.Context, root, name string, tree map[string]frontmatterTreeEntry) ([]byte, bool, error) {
	return frontmatterPreviousWithCaseSemantics(ctx, root, name, tree, filepath.Separator == '\\')
}

func frontmatterPreviousWithCaseSemantics(ctx context.Context, root, name string, tree map[string]frontmatterTreeEntry, caseInsensitiveFilesystem bool) ([]byte, bool, error) {
	changed := false
	var resolved []string
	pending := strings.Split(name, "/")
	links := 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		switch component {
		case "", ".":
			continue
		case "..":
			if len(resolved) == 0 {
				return nil, false, fmt.Errorf("%w: historical symlink %s escapes the repository", errFrontmatterHistoricalTargetUnavailable, name)
			}
			resolved = resolved[:len(resolved)-1]
			continue
		}

		candidateParts := append(append([]string(nil), resolved...), component)
		candidate := strings.Join(candidateParts, "/")
		entry, ok := frontmatterTreeLookup(tree, candidate, caseInsensitiveFilesystem)
		if !ok {
			if len(pending) > 0 && frontmatterTreeHasDirectoryWithCaseSemantics(tree, candidate, caseInsensitiveFilesystem) {
				resolved = candidateParts
				continue
			}
			return nil, false, fmt.Errorf("%w: missing historical symlink target %s", errFrontmatterHistoricalTargetUnavailable, candidate)
		}
		changed = changed || entry.changed
		if entry.mode == "120000" {
			links++
			if links > 40 {
				return nil, false, fmt.Errorf("%w: too many historical symlinks", errFrontmatterHistoricalTargetUnavailable)
			}
			target, err := frontmatterGit(ctx, root, "cat-file", "blob", entry.hash)
			if err != nil {
				return nil, false, err
			}
			targetName, absolute, err := frontmatterRepositoryTarget(root, string(target))
			if errors.Is(err, errFrontmatterSymlinkOutside) {
				return nil, false, fmt.Errorf("%w: historical symlink %s escapes the repository", errFrontmatterHistoricalTargetUnavailable, candidate)
			}
			if err != nil {
				return nil, false, err
			}
			// Preserve . and .. traversal after an absolute repository prefix.
			if absolute {
				resolved = nil
			} else {
				resolved = candidateParts[:len(candidateParts)-1]
			}
			pending = append(strings.Split(targetName, "/"), pending...)
			continue
		}
		if len(pending) > 0 {
			return nil, false, fmt.Errorf("%w: historical path component %s is not a directory", errFrontmatterHistoricalTargetUnavailable, candidate)
		}
		data, err := frontmatterGit(ctx, root, "cat-file", "blob", entry.hash)
		return data, changed, err
	}
	return nil, false, fmt.Errorf("%w: missing historical symlink target %s", errFrontmatterHistoricalTargetUnavailable, name)
}

func frontmatterTreeLookup(tree map[string]frontmatterTreeEntry, name string, caseInsensitiveFilesystem bool) (frontmatterTreeEntry, bool) {
	if entry, ok := tree[name]; ok {
		return entry, true
	}
	if caseInsensitiveFilesystem {
		for indexedName, entry := range tree {
			if strings.EqualFold(indexedName, name) {
				return entry, true
			}
		}
	}
	return frontmatterTreeEntry{}, false
}

func frontmatterTreeHasDirectoryWithCaseSemantics(tree map[string]frontmatterTreeEntry, dir string, caseInsensitiveFilesystem bool) bool {
	prefix := dir + "/"
	if !caseInsensitiveFilesystem {
		for name := range tree {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	dirParts := strings.Split(dir, "/")
	for name := range tree {
		parts := strings.Split(name, "/")
		if len(parts) <= len(dirParts) {
			continue
		}
		matches := true
		for i, part := range dirParts {
			if !strings.EqualFold(parts[i], part) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

var errFrontmatterHistoricalTargetUnavailable = errors.New("historical symlink target unavailable")

func runFrontmatter(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("kennlint frontmatter", flag.ContinueOnError)
	flags.SetOutput(stderr)
	changed := flags.String("changed", "", "require a last_edited change since the merge base with this ref")
	fix := flags.Bool("fix", false, "unquote dates and add missing last_edited using the last commit date (or today)")
	excludes := frontmatterExcludes{
		"README.md", "AGENTS.md", "CLAUDE.md", "docs/releases/**",
		"**/CHANGELOG*.md", "**/changelog*.md",
		".github/**", "**/testdata/**", "vendor/**", "node_modules/**",
	}
	var customExcludes frontmatterExcludes
	flags.Var(&customExcludes, "exclude", "additional repository-relative glob to skip; ** matches directories (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 0, err
	}
	rootBytes, err := frontmatterGit(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return 0, err
	}
	root := strings.TrimSpace(string(rootBytes))
	tracked, err := frontmatterGit(ctx, root, "ls-files", "-z", "--cached", "--deduplicate", "--", "*.md")
	if err != nil {
		return 0, err
	}
	indexModes, err := frontmatterIndexModes(ctx, root)
	if err != nil {
		return 0, err
	}
	symlinkPlaceholders, err := frontmatterSymlinkPlaceholders(ctx, root)
	if err != nil {
		return 0, err
	}
	caseInsensitiveFilesystem, err := frontmatterCaseInsensitiveFilesystem(ctx, root)
	if err != nil {
		return 0, err
	}
	skillDirs := frontmatterSkillDirectories(tracked)
	excludes = append(excludes, customExcludes...)
	selected, err := frontmatterPaths(root, flags.Args())
	if err != nil {
		return 0, err
	}
	var tree map[string]frontmatterTreeEntry
	if *changed != "" {
		base, err := frontmatterGit(ctx, root, "merge-base", "HEAD", *changed)
		if err != nil {
			return 0, err
		}
		tree, err = frontmatterTree(ctx, root, strings.TrimSpace(string(base)))
		if err != nil {
			return 0, err
		}
	}
	total, checked, fixed := 0, 0, 0
	for name := range bytes.SplitSeq(bytes.TrimSuffix(tracked, []byte{0}), []byte{0}) {
		file := string(name)
		if file == "" || !frontmatterSelected(file, selected, excludes) {
			continue
		}
		if path.Base(file) == "SKILL.md" {
			problems, err := checkSkillFrontmatterFileWithCaseSemantics(root, file, indexModes, symlinkPlaceholders, caseInsensitiveFilesystem)
			if err != nil {
				return total, fmt.Errorf("%s: %w", file, err)
			}
			checked++
			for _, problem := range problems {
				fmt.Fprintf(stderr, "%s: %s\n", file, strings.ReplaceAll(problem, "\n", "; "))
				total++
			}
			continue
		}
		if frontmatterInSkillDirectory(file, skillDirs) {
			continue
		}
		problems, didFix, err := checkFrontmatterFileWithCaseSemantics(ctx, root, file, *fix, tree, indexModes, symlinkPlaceholders, caseInsensitiveFilesystem)
		if err != nil {
			return total, fmt.Errorf("%s: %w", file, err)
		}
		checked++
		if didFix {
			fixed++
		}
		for _, problem := range problems {
			fmt.Fprintf(stderr, "%s: %s\n", file, strings.ReplaceAll(problem, "\n", "; "))
			total++
		}
	}
	if total == 0 {
		fmt.Fprintf(stdout, "frontmatter: checked %d Markdown files (%d fixed)\n", checked, fixed)
	}
	return total, nil
}

func frontmatterPaths(root string, args []string) ([]string, error) {
	var selected []string
	for _, arg := range args {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("path %q is outside the repository", arg)
		}
		selected = append(selected, filepath.ToSlash(rel))
	}
	return selected, nil
}

func frontmatterSelected(file string, selected []string, excludes []string) bool {
	if slices.ContainsFunc(excludes, func(pattern string) bool { return frontmatterGlob(pattern, file) }) {
		return false
	}
	return len(selected) == 0 || slices.ContainsFunc(selected, func(p string) bool {
		return p == "." || file == p || strings.HasPrefix(file, p+"/")
	})
}

func checkFrontmatterFileWithCaseSemantics(ctx context.Context, root, file string, fix bool, tree map[string]frontmatterTreeEntry, indexModes map[string]string, symlinkPlaceholders, caseInsensitiveFilesystem bool) ([]string, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(file))
	_, err := os.Lstat(full)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil // A tracked file deleted in the working tree needs no metadata.
	}
	if err != nil {
		return nil, false, err
	}
	targetPath, err := frontmatterWorkingPathWithCaseSemantics(root, file, indexModes, symlinkPlaceholders, caseInsensitiveFilesystem)
	if errors.Is(err, errFrontmatterSymlinkOutside) {
		return []string{"Markdown symlink target is outside the repository"}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	src, err := os.ReadFile(targetPath)
	if err != nil {
		return nil, false, err
	}
	b, err := parseFrontmatter(src)
	if fix && errors.Is(err, errFrontmatterMissing) {
		b, err = parseFrontmatter(append([]byte("---\n{}\n---\n"), src...))
		if err == nil {
			b.doc.Content[0].Style = 0
		}
	}
	if err != nil {
		return []string{err.Error()}, false, nil //nolint:nilerr // Count invalid metadata as a finding so scanning continues.
	}
	didFix := false
	if fix {
		date := time.Now().UTC().Format(time.DateOnly)
		if b.fields["last_edited"] == nil {
			// rev-parse --revs-only returns no revisions for an unborn HEAD.
			// Such a repository has no last commit date to ask git log for.
			head, err := frontmatterGit(ctx, root, "rev-parse", "--revs-only", "HEAD")
			if err != nil {
				return nil, false, err
			}
			if len(bytes.TrimSpace(head)) != 0 {
				committed, err := frontmatterGit(ctx, root, "log", "-1", "--format=%cs", "--", file)
				if err != nil {
					return nil, false, err
				}
				if value := strings.TrimSpace(string(committed)); value != "" {
					date = value
				}
			}
		}
		updated, changed, err := b.fix(date)
		if err != nil {
			return nil, false, err
		}
		if changed {
			opts := []atomicfile.Option{atomicfile.WithPreserveMode()}
			// Write the resolved file directly, preserving every alias and any
			// regular-file placeholders Git created for symlinks.
			if err := atomicfile.WriteFile(targetPath, updated, opts...); err != nil {
				return nil, false, err
			}
			didFix = true
		}
	}
	problems := b.problems()
	if _, exists := tree[file]; exists {
		previous, previousChanged, err := frontmatterPreviousWithCaseSemantics(ctx, root, file, tree, caseInsensitiveFilesystem)
		if errors.Is(err, errFrontmatterHistoricalTargetUnavailable) {
			return problems, didFix, nil
		}
		if err != nil {
			return nil, didFix, err
		}
		old, err := parseFrontmatter(previous)
		if err == nil && ((previousChanged && !bytes.Equal(previous, src)) || didFix) {
			before, after := old.fields["last_edited"], b.fields["last_edited"]
			if before != nil && after != nil && before.Value == after.Value {
				problems = append(problems, "content changed without changing last_edited")
			}
		}
	}
	return problems, didFix, nil
}
