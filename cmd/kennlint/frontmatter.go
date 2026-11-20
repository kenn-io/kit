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
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/consolewindow"
	gitenv "go.kenn.io/kit/git/env"
	"go.kenn.io/kit/okf"
)

// trimmedText is a flag whose value, once set, is non-empty after trimming.
type trimmedText struct{ value string }

func (t *trimmedText) String() string { return t.value }
func (t *trimmedText) Set(s string) error {
	t.value = strings.TrimSpace(s)
	if t.value == "" {
		return errors.New("value must not be empty")
	}
	return nil
}

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
	cmd := consolewindow.CommandContext(ctx, "git", args...)
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

func frontmatterInSkillDirectory(file string, skillDirs map[string]struct{}) bool {
	for dir := path.Dir(file); dir != "."; dir = path.Dir(dir) {
		if _, ok := skillDirs[dir]; ok {
			return true
		}
	}
	return false
}

// frontmatterTracked lists tracked Markdown files and the directories that
// hold a SKILL.md.
func frontmatterTracked(ctx context.Context, root string) ([]string, map[string]struct{}, error) {
	data, err := frontmatterGit(ctx, root, "ls-files", "-z", "--cached", "--deduplicate", "--", "*.md")
	if err != nil {
		return nil, nil, err
	}
	files := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(files) == 1 && files[0] == "" {
		files = nil
	}
	skillDirs := make(map[string]struct{})
	for _, name := range files {
		if path.Base(name) == "SKILL.md" && path.Dir(name) != "." {
			skillDirs[path.Dir(name)] = struct{}{}
		}
	}
	return files, skillDirs, nil
}

func runFrontmatter(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("kennlint frontmatter", flag.ContinueOnError)
	flags.SetOutput(stderr)
	changed := flags.String("changed", "", "require metadata to change for meaningful edits since this ref")
	legacy := flags.Bool("legacy", false, "use the temporary last_edited profile")
	var by, description trimmedText
	flags.Var(&by, "by", "actor for missing generated.by (producer/version, human:id, or process:id)")
	flags.Var(&description, "description", "description for one selected file")
	var rules frontmatterRules
	flags.Var(&rules, "type-rule", "path rule GLOB=TYPE (repeatable)")
	fix := flags.Bool("fix", false, "fill missing metadata where possible")
	excludes := frontmatterExcludes{
		"**/README.md", "**/AGENTS.md", "**/CLAUDE.md", "**/SKILL.md", "docs/releases/**",
		"**/CHANGELOG*.md", "**/changelog*.md",
		"**/release-notes.md", "**/RELEASE-NOTES.md", "**/release_notes.md", "**/RELEASE_NOTES.md",
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
	// Only the legacy fixer falls back to the last commit date. An unborn
	// branch has no history, even for a file already in the index.
	hasHead := false
	if *legacy && *fix {
		head, err := frontmatterGit(ctx, root, "rev-parse", "--revs-only", "HEAD")
		if err != nil {
			return 0, err
		}
		hasHead = len(bytes.TrimSpace(head)) != 0
	}

	var targets []string
	for _, file := range files {
		if frontmatterSelected(file, selected, excludes) && !frontmatterInSkillDirectory(file, skillDirs) {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(file))); !errors.Is(err, fs.ErrNotExist) {
				targets = append(targets, file)
			}
		}
	}
	if description.value != "" && len(targets) != 1 {
		return 0, errors.New("-description requires exactly one selected file")
	}
	total, fixed := 0, 0
	report := func(file string, problems []string) {
		for _, problem := range problems {
			fmt.Fprintf(stderr, "%s: %s\n", file, strings.ReplaceAll(problem, "\n", "; "))
			total++
		}
	}
	var checked []string
	for _, file := range targets {
		problems, didFix, err := checkFrontmatterFile(ctx, root, file, *fix, hasHead, *legacy, rules, by.value, description.value)
		if errors.Is(err, fs.ErrNotExist) {
			continue // A tracked file deleted in the working tree needs no metadata.
		}
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
		stale, err := frontmatterStaleDates(ctx, root, *changed, checked, *legacy)
		if err != nil {
			return total, err
		}
		for _, file := range stale {
			message := "content changed without a later generated.at; set generated.at to the edit time and generated.by to the actual editor"
			if *legacy {
				message = "content changed without changing last_edited"
			}
			report(file, []string{message})
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

func checkFrontmatterFile(ctx context.Context, root, file string, fix, hasHead, legacy bool, rules frontmatterRules, by, description string) ([]string, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(file))
	src, err := os.ReadFile(full)
	if err != nil {
		return nil, false, err
	}
	if !legacy {
		switch path.Base(file) {
		case "log.md":
			if bytes.HasPrefix(src, []byte("---\n")) || bytes.HasPrefix(src, []byte("---\r\n")) {
				return []string{"log.md must not have frontmatter"}, false, nil
			}
			return nil, false, nil
		case "index.md":
			// Reserved indexes are not concepts. A bundle root may carry metadata;
			// validation and index regeneration leave it untouched.
			_, err := okf.Parse(src)
			if err != nil && !errors.Is(err, okf.ErrNoFrontmatter) {
				return []string{err.Error()}, false, nil
			}
			return nil, false, nil
		}
	}
	b, err := okf.Parse(src)
	if fix && errors.Is(err, okf.ErrNoFrontmatter) {
		b, err = okf.New(src), nil
	}
	if err != nil {
		return []string{err.Error()}, false, nil //nolint:nilerr // Count invalid metadata as a finding so scanning continues.
	}
	var problems []string
	var changed bool
	if legacy {
		if fix {
			date := time.Now().UTC().Format(time.DateOnly)
			if b.Field("last_edited") == nil && hasHead {
				committed, err := frontmatterGit(ctx, root, "log", "-1", "--format=%cs", "--", file)
				if err != nil {
					return nil, false, err
				}
				if value := strings.TrimSpace(string(committed)); value != "" {
					date = value
				}
			}
			changed = fixLegacyDate(b, date)
		}
		problems = legacyProblems(b)
	} else {
		var notes map[string]string
		if fix {
			changed, notes = fixOKF(b, file, rules, by, description)
		}
		problems = frontmatterFixProblems(profileProblems(b), notes)
	}
	if !changed {
		return problems, false, nil
	}
	updated, err := b.Marshal()
	if err != nil {
		return nil, false, err
	}
	// A Markdown symlink is fixed like any path: the file it names changes and
	// the link stays.
	if err := atomicfile.WriteFile(full, updated, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink()); err != nil {
		return nil, false, err
	}
	return problems, true, nil
}

// Fixer guidance replaces the generic finding for the same field.
func frontmatterFixProblems(problems []string, notes map[string]string) []string {
	for i, problem := range problems {
		if note, ok := notes[strings.Fields(problem)[0]]; ok {
			problems[i] = note
		}
	}
	return problems
}

// frontmatterStaleDates returns the files whose content differs from the merge
// base with ref while the profile timestamp still matches. It runs after -fix so a fix
// that restores the base content does not count as an edit, and it lets Git
// apply attributes and line-ending rules so CRLF checkouts are not edits.
func frontmatterStaleDates(ctx context.Context, root, ref string, files []string, legacy bool) ([]string, error) {
	baseBytes, err := frontmatterGit(ctx, root, "merge-base", "HEAD", ref)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(string(baseBytes))
	diff, err := frontmatterGit(ctx, root, "diff", "--numstat", "-z", "--no-renames", "--diff-filter=M", base, "--", "*.md")
	if err != nil {
		return nil, err
	}
	modified := make(map[string]bool)
	for record := range bytes.SplitSeq(diff, []byte{0}) {
		// A mode-only change, such as setting the executable bit, is listed
		// as modified with zero added and deleted lines.
		added, rest, _ := strings.Cut(string(record), "\t")
		deleted, name, _ := strings.Cut(rest, "\t")
		if added != "0" || deleted != "0" {
			modified[name] = true
		}
	}
	var stale []string
	for _, file := range files {
		if !legacy && (path.Base(file) == "index.md" || path.Base(file) == "log.md") {
			continue // Reserved files are never concepts, even with extension metadata.
		}
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
		before, errBefore := okf.Parse(previous)
		after, errAfter := okf.Parse(current)
		if errBefore != nil || errAfter != nil {
			continue // Missing or invalid metadata is reported, or was adopted in this change.
		}
		if legacy {
			b, a := before.Field("last_edited"), after.Field("last_edited")
			if b != nil && a != nil && b.Value == a.Value {
				stale = append(stale, file)
			}
			continue
		}
		beforeTime, validBefore := okf.Timestamp(before.Field("generated", "at"))
		afterTime, validAfter := okf.Timestamp(after.Field("generated", "at"))
		if !validBefore || !validAfter || afterTime.After(beforeTime) {
			continue
		}
		meaningful, err := frontmatterMeaningful(before, after)
		if err != nil {
			return nil, fmt.Errorf("comparing %s: %w", file, err)
		}
		if meaningful {
			stale = append(stale, file)
		}
	}
	return stale, nil
}
