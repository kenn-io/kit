package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

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

func frontmatterInSkillDirectory(file string, skillDirs map[string]struct{}) bool {
	for dir := path.Dir(file); dir != "."; dir = path.Dir(dir) {
		if _, ok := skillDirs[dir]; ok {
			return true
		}
	}
	return false
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

// frontmatterTracked lists tracked regular Markdown files and the directories
// that hold a SKILL.md. Symlinks (mode 120000) are skipped: a target inside the
// repository is tracked and checked under its own name, and Git records the
// mode even when core.symlinks=false checks links out as plain files.
func frontmatterTracked(ctx context.Context, root string) ([]string, map[string]struct{}, error) {
	data, err := frontmatterGit(ctx, root, "ls-files", "-z", "--stage", "--", "*.md")
	if err != nil {
		return nil, nil, err
	}
	var files []string
	skillDirs := make(map[string]struct{})
	for record := range bytes.SplitSeq(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		metadata, name, ok := strings.Cut(string(record), "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			continue
		}
		if path.Base(name) == "SKILL.md" && path.Dir(name) != "." {
			skillDirs[path.Dir(name)] = struct{}{}
		}
		// Unmerged files appear once per stage, adjacently; check them once.
		if (fields[0] == "100644" || fields[0] == "100755") && (len(files) == 0 || files[len(files)-1] != name) {
			files = append(files, name)
		}
	}
	return files, skillDirs, nil
}

func runFrontmatter(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("kennlint frontmatter", flag.ContinueOnError)
	flags.SetOutput(stderr)
	changed := flags.String("changed", "", "require a last_edited change since the merge base with this ref")
	fix := flags.Bool("fix", false, "unquote dates and add missing last_edited using the last commit date (or today)")
	excludes := frontmatterExcludes{
		"**/README.md", "**/AGENTS.md", "**/CLAUDE.md", "**/SKILL.md", "docs/releases/**",
		"**/CHANGELOG*.md", "**/changelog*.md",
		".github/**", "**/testdata/**", "vendor/**", "node_modules/**",
	}
	var customExcludes frontmatterExcludes
	flags.Var(&customExcludes, "exclude", "additional repository-relative glob to skip; ** matches directories (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 0, err
	}
	excludes = append(excludes, customExcludes...)
	rootBytes, err := frontmatterGit(ctx, "", "rev-parse", "--show-toplevel")
	if err != nil {
		return 0, err
	}
	root := strings.TrimSpace(string(rootBytes))
	selected, err := frontmatterPaths(root, flags.Args())
	if err != nil {
		return 0, err
	}
	files, skillDirs, err := frontmatterTracked(ctx, root)
	if err != nil {
		return 0, err
	}
	// rev-parse --revs-only returns no revisions for an unborn HEAD, which has
	// no commit dates for -fix to use.
	head, err := frontmatterGit(ctx, root, "rev-parse", "--revs-only", "HEAD")
	if err != nil {
		return 0, err
	}
	hasHead := len(bytes.TrimSpace(head)) != 0

	total, fixed := 0, 0
	report := func(file string, problems []string) {
		for _, problem := range problems {
			fmt.Fprintf(stderr, "%s: %s\n", file, strings.ReplaceAll(problem, "\n", "; "))
			total++
		}
	}
	var checked []string
	for _, file := range files {
		if !frontmatterSelected(file, selected, excludes) || frontmatterInSkillDirectory(file, skillDirs) {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(file))
		info, err := os.Lstat(full)
		if errors.Is(err, os.ErrNotExist) {
			continue // A tracked file deleted in the working tree needs no metadata.
		}
		if err != nil {
			return total, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			continue // An uncommitted symlink; writing through it could reach anywhere.
		}
		problems, didFix, err := checkFrontmatterFile(ctx, root, file, *fix, hasHead)
		if err != nil {
			return total, fmt.Errorf("%s: %w", file, err)
		}
		checked = append(checked, file)
		if didFix {
			fixed++
		}
		report(file, problems)
	}
	if *changed != "" {
		stale, err := frontmatterStaleDates(ctx, root, *changed, checked)
		if err != nil {
			return total, err
		}
		for _, file := range stale {
			report(file, []string{"content changed without changing last_edited"})
		}
	}
	if total == 0 {
		fmt.Fprintf(stdout, "frontmatter: checked %d Markdown files (%d fixed)\n", len(checked), fixed)
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

func checkFrontmatterFile(ctx context.Context, root, file string, fix, hasHead bool) ([]string, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(file))
	src, err := os.ReadFile(full)
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
	if !fix {
		return b.problems(), false, nil
	}
	date := time.Now().UTC().Format(time.DateOnly)
	if b.fields["last_edited"] == nil && hasHead {
		committed, err := frontmatterGit(ctx, root, "log", "-1", "--format=%cs", "--", file)
		if err != nil {
			return nil, false, err
		}
		if value := strings.TrimSpace(string(committed)); value != "" {
			date = value
		}
	}
	updated, changed, err := b.fix(date)
	if err != nil || !changed {
		return b.problems(), false, err
	}
	if err := atomicfile.WriteFile(full, updated, atomicfile.WithPreserveMode()); err != nil {
		return nil, false, err
	}
	return b.problems(), true, nil
}

// frontmatterStaleDates returns the files whose content differs from the merge
// base with ref while last_edited still matches. It runs after -fix so a fix
// that restores the base content does not count as an edit, and it lets Git
// apply attributes and line-ending rules so CRLF checkouts are not edits.
func frontmatterStaleDates(ctx context.Context, root, ref string, files []string) ([]string, error) {
	baseBytes, err := frontmatterGit(ctx, root, "merge-base", "HEAD", ref)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(string(baseBytes))
	diff, err := frontmatterGit(ctx, root, "diff", "--name-only", "-z", "--no-renames", "--diff-filter=M", base, "--", "*.md")
	if err != nil {
		return nil, err
	}
	modified := make(map[string]bool)
	for name := range bytes.SplitSeq(diff, []byte{0}) {
		modified[string(name)] = true
	}
	var stale []string
	for _, file := range files {
		if !modified[file] {
			continue
		}
		previous, err := frontmatterGit(ctx, root, "cat-file", "blob", base+":"+file)
		if err != nil {
			return nil, err
		}
		current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if err != nil {
			return nil, err
		}
		before, errBefore := parseFrontmatter(previous)
		after, errAfter := parseFrontmatter(current)
		if errBefore != nil || errAfter != nil {
			continue // Missing or invalid metadata is reported, or was adopted in this change.
		}
		if b, a := before.fields["last_edited"], after.fields["last_edited"]; b != nil && a != nil && b.Value == a.Value {
			stale = append(stale, file)
		}
	}
	return stale, nil
}
