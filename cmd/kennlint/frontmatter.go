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

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/consolewindow"
	gitenv "go.kenn.io/kit/git/env"
	"go.kenn.io/kit/okf"
	"go.kenn.io/kit/pathresolve"
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
func isSkillFile(file string) bool {
	return path.Base(file) == "SKILL.md"
}

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
		if isSkillFile(name) {
			if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); errors.Is(err, fs.ErrNotExist) {
				continue
			} else if err != nil {
				return nil, nil, fmt.Errorf("%s: inspect skill boundary: %w", name, err)
			}
			skillDirs[path.Dir(name)] = struct{}{}
		}
	}
	return files, skillDirs, nil
}

func runFrontmatter(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	flags := flag.NewFlagSet("kennlint frontmatter", flag.ContinueOnError)
	flags.SetOutput(stderr)
	changed := flags.String("changed", "", "require metadata to change for meaningful edits since this ref")
	by := flags.String("by", "", "with -fix, actor for an absent generated mapping (producer/version, human:id, or process:id)")
	description := flags.String("description", "", "with -fix, description for one selected file")
	var rules frontmatterRules
	flags.Var(&rules, "type-rule", "path rule GLOB=TYPE (repeatable)")
	fix := flags.Bool("fix", false, "fill missing metadata where possible")
	excludes := frontmatterExcludes{
		"**/README.md", "**/AGENTS.md", "**/CLAUDE.md", "docs/releases/**",
		"**/CHANGELOG*.md", "**/changelog*.md",
		"**/release-notes.md", "**/RELEASE-NOTES.md", "**/release_notes.md", "**/RELEASE_NOTES.md",
		".github/**", "**/testdata/**", "vendor/**", "node_modules/**",
	}
	var managedSkills frontmatterExcludes
	flags.Var(&managedSkills, "managed-skill", "managed SKILL.md glob (repeatable)")
	var customExcludes frontmatterExcludes
	flags.Var(&customExcludes, "exclude", "additional repository-relative glob to skip; ** matches directories (repeatable)")
	if err := flags.Parse(args); err != nil {
		return 0, err
	}
	excludes = append(excludes, customExcludes...)
	*by, *description = strings.TrimSpace(*by), strings.TrimSpace(*description)
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
	var targets []string
	var eligibleSkills, owners []string
	docCount := 0
	_, rootSkill := skillDirs["."]
	// With an opted-in root skill, a scoped change check treats descendants as
	// resources, including when the selected resource has already been removed.
	rootResources := rootSkill && *changed != "" && len(selected) != 0 && !slices.Contains(selected, ".") &&
		frontmatterSelected("SKILL.md", nil, excludes) &&
		slices.ContainsFunc(managedSkills, func(pattern string) bool { return frontmatterGlob(pattern, "SKILL.md") })

	for _, file := range files {
		if !frontmatterSelected(file, nil, excludes) {
			continue
		}
		skill := isSkillFile(file)
		inScope := frontmatterSelected(file, selected, nil)
		if !skill && !inScope {
			continue
		}
		if rootSkill && file != "SKILL.md" && (*fix || rootResources || len(selected) == 0 || slices.Contains(selected, ".") || skill) {
			continue // Root-bundle descendants are writable only as the listed root skill.
		}
		container := file
		if skill {
			container = path.Dir(file)
		}
		if frontmatterInSkillDirectory(container, skillDirs) {
			continue
		}
		// Symlinked skills and indexes are imports and never checked as owned.
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(file)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("%s: inspect selected path: %w", file, err)
		}
		if info.Mode()&os.ModeSymlink != 0 && (skill || path.Base(file) == "index.md") {
			continue
		}
		if skill {
			eligibleSkills = append(eligibleSkills, file)
		}
		optedIn := skill && slices.ContainsFunc(managedSkills, func(pattern string) bool { return frontmatterGlob(pattern, file) })
		if (skill && !optedIn) || (!inScope && *changed == "") {
			continue
		}
		beyond, resolveErr := frontmatterResolvesBeyondBoundary(root, file, skillDirs, excludes)
		if resolveErr != nil {
			return 0, resolveErr
		}
		if beyond {
			continue
		}
		if skill {
			owners = append(owners, file)
		}
		if inScope {
			if !skill {
				docCount++
			}
			targets = append(targets, file)
		}
	}
	for _, pattern := range managedSkills {
		if !slices.ContainsFunc(eligibleSkills, func(file string) bool { return frontmatterGlob(pattern, file) }) {
			return 0, fmt.Errorf("-managed-skill pattern %q matches no tracked SKILL.md", pattern)
		}
	}
	if rootSkill && len(targets) == 0 && !rootResources {
		return 0, errors.New("root SKILL.md makes document scope ambiguous: select ordinary document paths for read-only checks; -fix protects all root-bundle descendants")
	}
	if *description != "" && docCount != 1 {
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
	reported := make(map[string]bool)
	for _, file := range targets {
		problems, didFix, err := checkFrontmatterFile(root, file, *fix, rules, *by, *description)
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
		reported[file] = len(problems) > 0
	}
	indexFixed, err := frontmatterUpdateIndexes(root, files, selected, skillDirs, excludes, *fix, reported, report)
	if err != nil {
		return total, err
	}
	fixed += indexFixed
	if *changed != "" {
		stale, err := frontmatterStaleDates(ctx, root, *changed, checked, owners, selected)
		if err != nil {
			return total, err
		}
		for _, s := range stale {
			if len(s.problems) != 0 {
				report(s.file, s.problems)
				continue
			}
			message := "content changed without a later generated.at; set generated.at to the edit time and generated.by to the actual editor"
			switch {
			case s.resource != "":
				message = "bundle file " + s.resource + " changed without a later metadata.generated.at; set metadata.generated.at to the edit time and metadata.generated.by to the actual editor"
			case isSkillFile(s.file):
				message = "content changed without a later metadata.generated.at; set metadata.generated.at to the edit time and metadata.generated.by to the actual editor"
			}
			report(s.file, []string{message})
		}
	}
	if total == 0 {
		fmt.Fprintf(stdout, "frontmatter: checked %d Markdown files (%d fixed)\n", len(checked), fixed)
	}
	return total, nil
}

// frontmatterResolvesBeyondBoundary reports whether file, once every symlink in
// its path (a linked ancestor directory as much as a linked final element) is
// followed, lands outside the repository, in an imported skill or skill
// bundle, or on an excluded path. Such a document, and any SKILL.md reached
// through an alias, is never read or rewritten through its alias; an eligible
// in-repository document alias stays supported.
func frontmatterResolvesBeyondBoundary(root, file string, skillDirs map[string]struct{}, excludes []string) (bool, error) {
	resolved, err := pathresolve.EvalSymlinks(filepath.Join(root, filepath.FromSlash(file)))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%s: resolve symlink: %w", file, err)
	}
	resolvedRoot, err := pathresolve.EvalSymlinks(root)
	if err != nil {
		return false, fmt.Errorf("resolve repository root: %w", err)
	}
	rel, relErr := filepath.Rel(resolvedRoot, resolved)
	inside := relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	target := filepath.ToSlash(rel)
	if target != file && (path.Base(target) == "log.md" || path.Base(target) == "index.md" || path.Base(file) == "index.md") {
		return true, nil // Reserved documents are only handled under their own names.
	}
	container := target
	if isSkillFile(target) {
		container = path.Dir(target) // A directly owned SKILL.md sits in its own bundle.
	}
	return !inside || frontmatterHiddenPath(target) || (isSkillFile(target) && target != file) || frontmatterInSkillDirectory(container, skillDirs) || !frontmatterSelected(target, nil, excludes), nil
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

func checkFrontmatterFile(root, file string, fix bool, rules frontmatterRules, by, description string) ([]string, bool, error) {
	full := filepath.Join(root, filepath.FromSlash(file))
	src, err := os.ReadFile(full)
	if err != nil {
		return nil, false, err
	}
	switch path.Base(file) {
	case "log.md":
		if bytes.HasPrefix(src, []byte("---\n")) || bytes.HasPrefix(src, []byte("---\r\n")) {
			return []string{"log.md must not have frontmatter"}, false, nil
		}
		return nil, false, nil
	case "index.md":
		// Reserved indexes are not concepts; any metadata only has to parse.
		_, err := okf.Parse(src)
		if err != nil && !errors.Is(err, okf.ErrNoFrontmatter) {
			return []string{err.Error()}, false, nil
		}
		return nil, false, nil
	}
	if isSkillFile(file) {
		return checkManagedSkill(full, src, fix, by)
	}
	b, err := okf.Parse(src)
	if fix && errors.Is(err, okf.ErrNoFrontmatter) {
		b, err = okf.New(src), nil
	}
	if err != nil {
		return []string{err.Error()}, false, nil //nolint:nilerr // Count invalid metadata as a finding so scanning continues.
	}
	changed := fix && fixOKF(b, file, rules, by, description)
	problems := profileProblems(b)
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

// frontmatterStale names a file whose timestamp missed a meaningful edit. For
// a managed skill, resource names the first changed bundle file when SKILL.md
// itself changed only cosmetically.
type frontmatterStale struct {
	file, resource string
	problems       []string
}

// frontmatterStaleDates returns the files whose content differs from the merge
// base with ref while the profile timestamp still matches. A managed skill also
// counts as changed when any tracked file it owns changed. It runs after
// -fix so a fix that restores the base content does not count as an edit, and
// it lets Git apply attributes and line-ending rules so CRLF checkouts are not
// edits.
func frontmatterStaleDates(ctx context.Context, root, ref string, files, owners, selected []string) ([]frontmatterStale, error) {
	baseBytes, err := frontmatterGit(ctx, root, "merge-base", "HEAD", ref)
	if err != nil {
		return nil, err
	}
	base := strings.TrimSpace(string(baseBytes))
	changes, err := frontmatterChanges(ctx, root, base)
	if err != nil {
		return nil, err
	}
	// Determine ownership from the opted-in skills, independently of path
	// selection. Only selected changes can pull an unselected owner into the
	// timestamp check. They do not become document targets or fixer targets.
	managed := make(map[string]struct{}, len(owners))
	for _, owner := range owners {
		managed[path.Dir(owner)] = struct{}{}
	}
	direct := make(map[string]bool, len(files))
	for _, file := range files {
		direct[file] = true
	}
	selectedChanges := make(map[string]string)
	inferred := make(map[string]bool)
	if len(selected) != 0 && len(owners) != 0 {
		for file, status := range changes {
			if !frontmatterSelected(file, selected, nil) {
				continue
			}
			selectedChanges[file] = status
			for dir := path.Dir(file); ; dir = path.Dir(dir) {
				if _, ok := managed[dir]; ok {
					owner := path.Join(dir, "SKILL.md")
					if file != owner {
						inferred[owner] = true
					}
					break
				}
				if dir == "." {
					break
				}
			}
		}
	}
	var stale []frontmatterStale
	for _, file := range files {
		if path.Base(file) == "index.md" || path.Base(file) == "log.md" {
			continue // Reserved files are never concepts, even with extension metadata.
		}
		skill := isSkillFile(file)
		status := changes[file]
		if status == "A" || (status != "M" && !skill) {
			continue // New files carry their first timestamp; untouched documents need none.
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
		keys := []string{"generated", "at"}
		if skill {
			keys = append([]string{"metadata"}, keys...)
		}
		beforeTime, validBefore := okf.Timestamp(before.Field(keys...))
		afterTime, validAfter := okf.Timestamp(after.Field(keys...))
		if !validBefore || !validAfter || afterTime.After(beforeTime) {
			continue
		}
		if status == "M" {
			meaningful, err := profileMeaningful(before, after, skill)
			if err != nil {
				return nil, fmt.Errorf("comparing %s: %w", file, err)
			}
			if meaningful {
				stale = append(stale, frontmatterStale{file: file})
				continue
			}
		}
		if skill {
			resource, err := frontmatterBundleChange(ctx, root, base, path.Dir(file), changes, managed)
			if err != nil {
				return nil, err
			}
			if resource != "" {
				stale = append(stale, frontmatterStale{file: file, resource: resource})
			}
		}
	}
	for _, owner := range owners {
		if !inferred[owner] || direct[owner] || changes[owner] == "A" {
			continue
		}
		finding, err := frontmatterIndirectSkill(ctx, root, base, owner, selectedChanges, managed)
		if err != nil {
			return nil, err
		}
		if finding != nil {
			stale = append(stale, *finding)
		}
	}
	return stale, nil
}

// Check only the provenance of an indirectly selected skill. The owner is
// never fixed, and first-time provenance adoption has no timestamp to advance.
func frontmatterIndirectSkill(ctx context.Context, root, base, owner string, changes map[string]string, managed map[string]struct{}) (*frontmatterStale, error) {
	resource, err := frontmatterBundleChange(ctx, root, base, path.Dir(owner), changes, managed)
	if err != nil || resource == "" {
		return nil, err
	}
	previous, err := frontmatterGit(ctx, root, "cat-file", "blob", base+":"+owner)
	if err != nil {
		return nil, err
	}
	before, err := okf.Parse(previous)
	if err != nil {
		return nil, nil //nolint:nilerr // A skill adopting provenance has no prior timestamp.
	}
	beforeTime, validBefore := okf.Timestamp(before.Field("metadata", "generated", "at"))
	if !validBefore {
		return nil, nil
	}
	current, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(owner)))
	if err != nil {
		return nil, err
	}
	finding := &frontmatterStale{file: owner, resource: resource}
	after, err := okf.Parse(current)
	if err != nil {
		finding.problems = []string{fmt.Sprintf("bundle file %s changed, but skill metadata cannot be read: %v", resource, err)}
		return finding, nil
	}
	if problems := generatedProblems(after.Field("metadata", "generated"), "metadata.generated"); len(problems) != 0 {
		finding.problems = problems
		return finding, nil
	}
	if afterTime, ok := okf.Timestamp(after.Field("metadata", "generated", "at")); ok && afterTime.After(beforeTime) {
		return nil, nil
	}
	return finding, nil
}

// frontmatterChanges maps each tracked path that differs from base to its
// one-letter status. A modification with no changed lines, such as setting
// the executable bit, is omitted; binary changes stay listed.
func frontmatterChanges(ctx context.Context, root, base string) (map[string]string, error) {
	statuses, err := frontmatterGit(ctx, root, "diff", "--name-status", "-z", "--no-renames", base)
	if err != nil {
		return nil, err
	}
	numstat, err := frontmatterGit(ctx, root, "diff", "--numstat", "-z", "--no-renames", "--diff-filter=M", base)
	if err != nil {
		return nil, err
	}
	changes := make(map[string]string)
	fields := strings.Split(strings.TrimSuffix(string(statuses), "\x00"), "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		changes[fields[i+1]] = fields[i][:1]
	}
	for record := range strings.SplitSeq(string(numstat), "\x00") {
		added, rest, _ := strings.Cut(record, "\t")
		deleted, name, _ := strings.Cut(rest, "\t")
		if added == "0" && deleted == "0" {
			delete(changes, name)
		}
	}
	return changes, nil
}

// frontmatterBundleChange returns the first tracked file owned by the managed
// skill in dir, other than its SKILL.md, whose change is
// meaningful: any addition, deletion, type change or byte change, except that
// Markdown edits must change the rendered content.
func frontmatterBundleChange(ctx context.Context, root, base, dir string, changes map[string]string, managed map[string]struct{}) (string, error) {
	var names []string
	for name := range changes {
		if dir != "." && !strings.HasPrefix(name, dir+"/") || name == path.Join(dir, "SKILL.md") {
			continue
		}
		if frontmatterNestedSkill(name, dir, managed) {
			continue // A managed nested skill owns its subtree and its own provenance.
		}
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if changes[name] != "M" || !strings.EqualFold(path.Ext(name), ".md") {
			return name, nil
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		if info, err := os.Lstat(full); err != nil || info.Mode()&os.ModeSymlink != 0 {
			return name, nil //nolint:nilerr // A retargeted link or unreadable change still counts as an edit.
		}
		previous, err := frontmatterGit(ctx, root, "cat-file", "blob", base+":"+name)
		if err != nil {
			return "", err
		}
		current, err := os.ReadFile(full)
		if err != nil {
			return "", err
		}
		if canonicalHTML(previous) != canonicalHTML(current) {
			return name, nil
		}
	}
	return "", nil
}

// frontmatterNestedSkill reports whether name lies in a managed skill
// directory strictly below dir.
func frontmatterNestedSkill(name, dir string, managed map[string]struct{}) bool {
	for d := path.Dir(name); d != dir && d != "."; d = path.Dir(d) {
		if _, ok := managed[d]; ok {
			return true
		}
	}
	return false
}
