package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	gittest "go.kenn.io/kit/git/test"
)

func TestRefreshIndex(t *testing.T) {
	tests := []struct {
		name, src, want string
		pages           []indexPage
		bad             bool
	}{
		{"empty", "before\n" + indexBegin + "\nold\n" + indexEnd + "\nafter\n", "before\n" + indexBegin + "\n" + indexEnd + "\nafter\n", nil, false},
		{"escaped CRLF", "top\r\n" + indexBegin + "\r\n" + indexEnd + "\r\nend", "top\r\n" + indexBegin + "\r\n- [A\\[b\\]\\\\c](a%20b/x.md) - two words\r\n- [Z](z.md) - last\r\n" + indexEnd + "\r\nend", []indexPage{{"a b/x.md", `A[b]\c`, "two\n words"}, {"z.md", "Z", "last"}}, false},
		{"fenced backticks", "```markdown\n" + indexBegin + "\nexample\n" + indexEnd + "\n```\n", "```markdown\n" + indexBegin + "\nexample\n" + indexEnd + "\n```\n", []indexPage{{"page.md", "Page", "Description"}}, false},
		{"fenced tildes and real block", "~~~markdown\n" + indexBegin + "\nexample\n" + indexEnd + "\n~~~\n" + indexBegin + "\nold\n" + indexEnd + "\n", "~~~markdown\n" + indexBegin + "\nexample\n" + indexEnd + "\n~~~\n" + indexBegin + "\n- [Page](page.md) - Description\n" + indexEnd + "\n", []indexPage{{"page.md", "Page", "Description"}}, false},
		{"unmarked", "untouched\n", "untouched\n", nil, false},
		{"missing end", indexBegin + "\n", "", nil, true},
		{"end first", indexEnd + "\n" + indexBegin + "\n", "", nil, true},
		{"duplicate", indexBegin + "\n" + indexEnd + "\n" + indexBegin + "\n" + indexEnd + "\n", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			index := filepath.Join(root, "index.md")
			require.NoError(t, os.WriteFile(index, []byte(tt.src), 0o600))
			files := []string{"index.md"}
			for _, page := range tt.pages {
				filename := filepath.Join(root, filepath.FromSlash(page.path))
				require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0o700))
				header := "---\ntitle: " + strconv.Quote(page.title) + "\ndescription: " + strconv.Quote(page.description) + "\n---\n"
				require.NoError(t, os.WriteFile(filename, []byte(header), 0o600))
				files = append(files, page.path)
			}
			got, changed, err := refreshTrackedIndex(root, "index.md", files, nil, nil)
			if tt.bad {
				require.Error(t, err)
				assert.Equal(t, tt.src, string(got))
				assert.False(t, changed)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
			assert.Equal(t, tt.want != tt.src, changed)
			require.NoError(t, os.WriteFile(index, got, 0o600))
			again, changed, err := refreshTrackedIndex(root, "index.md", files, nil, nil)
			require.NoError(t, err)
			assert.False(t, changed)
			assert.Equal(t, got, again)
		})
	}
}

func TestManagedIndexFlow(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("docs/index.md", "# Index\n"+indexBegin+"\n- [Missing](gone.md) - Gone\n"+indexEnd+"\n[Future](/not-yet-written.md)\n")
	repo.WriteFile("docs/a.md", goodFrontmatter)
	repo.WriteFile("docs/log.md", "no header\n")
	repo.WriteFile("docs/README.md", goodFrontmatter)
	repo.WriteFile("docs/testdata/hidden.md", goodFrontmatter)
	repo.WriteFile("docs/skill/SKILL.md", goodFrontmatter)
	repo.WriteFile("docs/skill/resource.md", goodFrontmatter)
	repo.WriteFile("docs/old.md", strings.Replace(goodFrontmatter, "title: A page", "status: deprecated\ntitle: Old", 1))
	repo.WriteFile("docs/untracked.md", goodFrontmatter)
	repo.Run("add", "docs/index.md", "docs/a.md", "docs/log.md", "docs/README.md", "docs/testdata/hidden.md", "docs/skill/SKILL.md", "docs/skill/resource.md", "docs/old.md")
	read := func() string {
		b, err := os.ReadFile(filepath.Join(repo.Root, "docs/index.md"))
		require.NoError(t, err)
		return string(b)
	}
	run := func(args ...string) (int, string) {
		var out, stderr bytes.Buffer
		n, err := runFrontmatter(t.Context(), args, &out, &stderr)
		require.NoError(t, err)
		return n, stderr.String()
	}
	before := read()
	n, diagnostic := run("docs/index.md")
	assert.Equal(t, 1, n)
	assert.Contains(t, diagnostic, "managed index is stale")
	assert.NotContains(t, diagnostic, "fix locally:")
	assert.Equal(t, before, read())
	n, diagnostic = run("-fix", "docs/index.md")
	require.Zero(t, n, diagnostic)
	assert.Contains(t, read(), "- [A page](a.md) - A useful page.")
	assert.NotContains(t, read(), "gone.md")
	assert.Contains(t, read(), "[Future](/not-yet-written.md)")
	for _, name := range []string{"log.md", "README.md", "hidden.md", "SKILL.md", "resource.md", "old.md", "untracked.md"} {
		assert.NotContains(t, read(), name)
	}
	n, diagnostic = run("docs/log.md")
	assert.Zero(t, n, diagnostic)
	n, diagnostic = run("-legacy", "docs/log.md")
	assert.Positive(t, n)
	assert.Contains(t, diagnostic, "missing opening")
}

func TestManagedIndexDescriptionRendersAsText(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.md"), []byte(indexBegin+"\n"+indexEnd+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "a.md"), []byte("---\ntitle: First\ndescription: '<count> *literal* [x](y) <!--'\n---\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "b.md"), []byte("---\ntitle: Second\ndescription: Visible\n---\n"), 0o600))
	got, _, err := refreshTrackedIndex(root, "index.md", []string{"index.md", "a.md", "b.md"}, nil, nil)
	require.NoError(t, err)
	var rendered bytes.Buffer
	require.NoError(t, html.New().Render(&rendered, got, parser.New().Parse(got)))
	assert.Contains(t, rendered.String(), `&lt;count&gt; *literal* [x](y) &lt;!--`)
	assert.Contains(t, rendered.String(), `<a href="b.md">Second</a> - Visible`)
	assert.NotContains(t, rendered.String(), "<em>")
	assert.NotContains(t, rendered.String(), `href="y"`)
	assert.Equal(t, `I/O kenn-io, 1.2 (ok)\!`, escapeIndexText("I/O kenn-io, 1.2 (ok)!"))
	tests := []struct {
		name, src, want string
		pages           []indexPage
	}{
		{"multiline title", indexBegin + "\n" + indexEnd + "\n", indexBegin + "\n- [First second third](page.md) - description\n" + indexEnd + "\n", []indexPage{{"page.md", "First\n\nsecond\tthird", "description"}}},
		{"punctuation title and colon path", indexBegin + "\n" + indexEnd + "\n", indexBegin + "\n- [Use \\*bold\\* \\[safely\\]\\!](./guide:setup.md) - description\n" + indexEnd + "\n", []indexPage{{"guide:setup.md", "Use *bold* [safely]!", "description"}}},
		{"GFM punctuation", indexBegin + "\n" + indexEnd + "\n", indexBegin + "\n- [Tilde \\~\\~text\\~\\~](page.md) - Emoji \\:smile\\:\n" + indexEnd + "\n", []indexPage{{"page.md", "Tilde ~~text~~", "Emoji :smile:"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if runtime.GOOS == "windows" && tt.name == "punctuation title and colon path" {
				t.Skip("colon is not valid in Windows filenames")
			}
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "index.md"), []byte(tt.src), 0o600))
			files := []string{"index.md"}
			for _, page := range tt.pages {
				header := "---\ntitle: " + strconv.Quote(page.title) + "\ndescription: " + strconv.Quote(page.description) + "\n---\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, page.path), []byte(header), 0o600))
				files = append(files, page.path)
			}
			got, changed, err := refreshTrackedIndex(root, "index.md", files, nil, nil)
			require.NoError(t, err)
			assert.True(t, changed)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestRootSkillSkipsManagedIndexes(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("references/example.md", goodOKF)
	const original = indexBegin + "\nold\n" + indexEnd + "\n"
	repo.WriteFile("index.md", original)
	repo.WriteFile("references/index.md", original)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	for _, file := range []string{"index.md", "references/index.md"} {
		got, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, original, string(got))
	}
	// Deleting the root skill restores ordinary document and index checks.
	require.NoError(t, os.Remove("SKILL.md"))
	out.Reset()
	errs.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-fix", "index.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	got, err := os.ReadFile("index.md")
	require.NoError(t, err)
	assert.Contains(t, string(got), "references/example.md")
	assert.NotContains(t, string(got), "old")
}

func TestManagedIndexSelectedPageChanges(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("docs/index.md", indexBegin+"\n"+indexEnd+"\n")
	repo.WriteFile("other/index.md", indexBegin+"\n"+indexEnd+"\n")
	repo.WriteFile("other/page.md", goodOKF)
	repo.WriteFile("docs/a.md", goodOKF)
	repo.Run("add", ".")
	read := func(name string) string {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		return string(data)
	}
	checkAndFix := func(selected string) {
		var out, errs bytes.Buffer
		before := read("docs/index.md")
		n, err := runFrontmatter(t.Context(), []string{selected}, &out, &errs)
		require.NoError(t, err)
		assert.Equal(t, 1, n, errs.String())
		assert.Contains(t, errs.String(), "docs/index.md: managed index is stale")
		assert.NotContains(t, errs.String(), "other/index.md")
		assert.Equal(t, before, read("docs/index.md"))
		out.Reset()
		errs.Reset()
		n, err = runFrontmatter(t.Context(), []string{"-fix", selected}, &out, &errs)
		require.NoError(t, err)
		require.Zero(t, n, errs.String())
		assert.NotContains(t, errs.String(), "other/index.md")
		assert.Equal(t, indexBegin+"\n"+indexEnd+"\n", read("other/index.md"))
	}
	checkAndFix("docs/a.md")
	assert.Contains(t, read("docs/index.md"), "[A page](a.md)")
	repo.WriteFile("docs/a.md", strings.Replace(goodOKF, "A page", "Edited", 1))
	checkAndFix("docs/a.md")
	assert.Contains(t, read("docs/index.md"), "[Edited](a.md)")
	repo.WriteFile("docs/b.md", strings.Replace(goodOKF, "A page", "Added", 1))
	repo.Run("add", "docs/b.md")
	checkAndFix("docs/b.md")
	assert.Contains(t, read("docs/index.md"), "[Added](b.md)")
	repo.Run("mv", "docs/b.md", "docs/c.md")
	checkAndFix("docs/c.md")
	assert.NotContains(t, read("docs/index.md"), "(b.md)")
	assert.Contains(t, read("docs/index.md"), "(c.md)")
	repo.Run("rm", "-f", "docs/c.md")
	checkAndFix("docs/c.md")
	assert.NotContains(t, read("docs/index.md"), "(c.md)")
}

func TestManagedIndexInvalidPageReportedOnce(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const original = indexBegin + "\nold\n" + indexEnd + "\n"
	repo.WriteFile("docs/index.md", original)
	repo.WriteFile("docs/bad.md", strings.Replace(goodOKF, "title: A page", "title: ''", 1))
	repo.Run("add", ".")
	for _, args := range [][]string{nil, {"-fix"}} {
		var out, errs bytes.Buffer
		n, err := runFrontmatter(t.Context(), args, &out, &errs)
		require.NoError(t, err)
		assert.Equal(t, 1, n, errs.String())
		assert.Contains(t, errs.String(), "docs/bad.md: title must be a non-empty string")
		assert.NotContains(t, errs.String(), "index page requires")
		got, err := os.ReadFile("docs/index.md")
		require.NoError(t, err)
		assert.Equal(t, original, string(got))
	}
	// Selecting only the index still reports the page and leaves the index alone.
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "docs/index.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, errs.String(), "index page requires a valid title and description")
	got, err := os.ReadFile("docs/index.md")
	require.NoError(t, err)
	assert.Equal(t, original, string(got))
}

func TestIndexV03BundleMetadata(t *testing.T) {
	root := t.TempDir()
	header := "---\nokf_version: \"0.3\"\ntitle: Bundle\ndescription: Useful docs\nlicense: /LICENSE\nx-index: {preserve: [one, 2, true]}\n---\n"
	src := header + "# Pages\n" + indexBegin + "\nold\n" + indexEnd + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "index.md"), []byte(src), 0o600))
	files := []string{"index.md"}
	for _, status := range []string{"stable", "draft", "deprecated", "future-status"} {
		file := status + ".md"
		require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(strings.Replace(goodOKF, "type: Guide\n", "type: Guide\nstatus: "+status+"\n", 1)), 0o600))
		files = append(files, file)
	}
	got, changed, err := refreshTrackedIndex(root, "index.md", files, nil, nil)
	require.NoError(t, err)
	require.True(t, changed)
	assert.True(t, bytes.HasPrefix(got, []byte(header)), "preserve bundle metadata byte for byte")
	assert.Contains(t, string(got), "(stable.md)")
	assert.Contains(t, string(got), "(draft.md)")
	assert.NotContains(t, string(got), "(deprecated.md)")
	assert.NotContains(t, string(got), "(future-status.md)")
}

func TestIndexResolvedSkillExclusions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const protected = "# Resource\n" + indexBegin + "\noriginal\n" + indexEnd + "\n"
	repo.WriteFile("skills/a/SKILL.md", "---\nname: example\ndescription: Example\n---\n# Skill\n")
	repo.WriteFile("skills/a/references/index.md", protected)
	repo.WriteFile("skills/a/references/page.md", "# Resource\n")
	repo.WriteFile("other/page.md", goodOKF)
	repo.WriteFile("excluded/index.md", protected)
	repo.WriteFile("excluded/page.md", "# Excluded\n")
	repo.WriteFile("docs/index.md", indexBegin+"\nold\n"+indexEnd+"\n")
	for name, target := range map[string]string{
		"docs/resource.md":           "../skills/a/references/page.md",
		"docs/skill-index/index.md":  "../../skills/a/references/index.md",
		"docs/ordinary.md":           "../other/page.md",
		"docs/custom.md":             "../excluded/page.md",
		"docs/custom-index/index.md": "../../excluded/index.md",
	} {
		require.NoError(t, os.MkdirAll(filepath.Dir(name), 0o700))
		require.NoError(t, os.Symlink(target, name))
	}
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-exclude", "excluded/**", "docs"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	got, err := os.ReadFile("docs/index.md")
	require.NoError(t, err)
	assert.Contains(t, string(got), "(ordinary.md)")
	assert.NotContains(t, string(got), "resource.md")
	assert.NotContains(t, string(got), "custom.md")
	for _, file := range []string{"skills/a/references/index.md", "excluded/index.md"} {
		got, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, protected, string(got), file)
	}
}

func TestIndexReviewBundleBoundaries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const protected = "# Imported resource\n" + indexBegin + "\nAuthored skill resource\n" + indexEnd + "\n"
	external := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(external, "references"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(external, "SKILL.md"), []byte(skillHeader), 0o600))
	target := filepath.Join(external, "references", "index.md")
	require.NoError(t, os.WriteFile(target, []byte(protected), 0o600))
	repo.WriteFile("docs/index.md", indexBegin+"\nold\n"+indexEnd+"\n")
	repo.WriteFile("docs/visible.md", goodOKF)
	repo.WriteFile("docs/.drafts/page.md", goodOKF)
	repo.WriteFile("docs/nested/.hidden/page.md", goodOKF)
	repo.WriteFile("docs/.page.md", goodOKF)
	repo.WriteFile("docs/.drafts/index.md", protected)
	require.NoError(t, os.MkdirAll("docs/import", 0o700))
	require.NoError(t, os.Symlink(target, "docs/import/index.md"))
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "docs"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, protected, string(got))
	got, err = os.ReadFile("docs/.drafts/index.md")
	require.NoError(t, err)
	assert.Equal(t, protected, string(got))
	got, err = os.ReadFile("docs/index.md")
	require.NoError(t, err)
	assert.Contains(t, string(got), "(visible.md)")
	assert.NotContains(t, string(got), ".drafts")
	assert.NotContains(t, string(got), ".hidden")
	assert.NotContains(t, string(got), ".page.md")
}
