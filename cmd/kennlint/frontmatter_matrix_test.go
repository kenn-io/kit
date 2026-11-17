package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

const goodOKF = "---\ntype: Guide\ntitle: A page\ndescription: Useful\ngenerated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n---\n\n# A page\n\nText here.\n"

func TestOKFValidationMatrix(t *testing.T) {
	fields := []struct{ old, invalid, problem string }{
		{"type: Guide\n", "", "type must"},
		{"type: Guide", "type: ''", "type must"},
		{"type: Guide", "type: 42", "type must"},
		{"title: A page\n", "", "title must"},
		{"title: A page", "title: ' ' ", "title must"},
		{"title: A page", "title: false", "title must"},
		{"description: Useful\n", "", "description must"},
		{"description: Useful", "description: ''", "description must"},
		{"description: Useful", "description: []", "description must"},
		{"  by: human:test\n", "", "generated.by"},
		{"by: human:test", "by: ''", "generated.by"},
		{"by: human:test", "by: 4", "generated.by"},
		{"  at: 2026-11-17T12:00:00Z\n", "", "generated.at"},
		{"at: 2026-11-17T12:00:00Z", "at: ''", "generated.at"},
		{"at: 2026-11-17T12:00:00Z", "at: 123", "generated.at"},
		{"2026-11-17T12:00:00Z", "2026-11-17T12:00:00+01:00", "generated.at"},
		{"2026-11-17T12:00:00Z", "2026-11-17", "generated.at"},
		{"2026-11-17T12:00:00Z", "2026-11-17T12:00:00", "generated.at"},
	}
	for i, tt := range fields {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, tt.old, tt.invalid, 1)))
			require.NoError(t, err)
			assert.Contains(t, strings.Join(b.okfProblems(), "\n"), tt.problem)
		})
	}
	for _, src := range []string{goodOKF, strings.ReplaceAll(goodOKF, "\n", "\r\n"), strings.Replace(goodOKF, "2026-11-17T12:00:00Z", "'2026-11-17T12:00:00.123Z'", 1), strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z", "generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}", 1), strings.Replace(goodOKF, "---\n\n#", "last_edited: 2020-01-01\n---\n\n#", 1)} {
		b, err := parseFrontmatter([]byte(src))
		require.NoError(t, err)
		assert.Empty(t, b.okfProblems())
	}
	b, err := parseFrontmatter([]byte("---\ntype: Guide\ntitle: A\ndescription: B\ngenerated: nope\n---\n"))
	require.NoError(t, err)
	assert.Contains(t, b.okfProblems(), "generated must be a mapping")
	for _, src := range []string{"---\ngenerated: {by: a, by: b}\n---\n", "---\ntitle: [\n---\n", "---\n- abc\n---\n"} {
		_, err := parseFrontmatter([]byte(src))
		assert.Error(t, err)
	}
}

func TestOKFFixMatrix(t *testing.T) {
	old := frontmatterNow
	frontmatterNow = func() time.Time { return time.Date(2026, 11, 17, 12, 34, 56, 123000000, time.UTC) }
	t.Cleanup(func() { frontmatterNow = old })
	cases := []struct {
		name                                                string
		rules                                               frontmatterRules
		existing, body, by, description, wantType, wantNote string
	}{
		{"autolink", frontmatterRules{{"docs/**", "Guide"}}, "", "# Connect to <https://example.com>\n", "human:test", "Useful", "Guide", ""},
		{"one", frontmatterRules{{"docs/**", "Guide"}}, "", "# **Hello** world\n", "human:test", "Useful", "Guide", ""},
		{"agree", frontmatterRules{{"docs/**", "Guide"}, {"**/*.md", "Guide"}}, "", "# Hello\n", "human:test", "Useful", "Guide", ""},
		{"ambiguous", frontmatterRules{{"docs/**", "Guide"}, {"**/*.md", "Reference"}}, "", "# Hello\n", "human:test", "Useful", "", "type rules disagree"},
		{"no rule", nil, "", "# Hello\n", "human:test", "Useful", "", "add type:"},
		{"existing", frontmatterRules{{"docs/**", "Guide"}}, "type: Reference\n", "# Hello\n", "human:test", "Useful", "Reference", ""},
		{"no heading", frontmatterRules{{"docs/**", "Guide"}}, "", "intro\n\n# Hello\n", "human:test", "Useful", "Guide", "first block"},
		{"no actor", frontmatterRules{{"docs/**", "Guide"}}, "", "# Hello\n", "", "Useful", "Guide", "-by ACTOR"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte("---\n" + tt.existing + "last_edited: 2025-01-01\n---\n" + tt.body))
			require.NoError(t, err)
			changed, notes := b.fixOKF("docs/file.md", tt.rules, tt.by, tt.description)
			require.True(t, changed)
			if tt.name == "autolink" {
				assert.Equal(t, "Connect to https://example.com", b.fields["title"].Value)
			}
			if tt.name == "one" {
				require.NotNil(t, b.fields["title"])
				assert.Equal(t, "Hello world", b.fields["title"].Value)
			}
			if tt.wantType != "" {
				require.NotNil(t, b.fields["type"])
				assert.Equal(t, tt.wantType, b.fields["type"].Value)
			}
			if tt.wantNote != "" {
				assert.Contains(t, strings.Join(notes, "\n"), tt.wantNote)
			}
			if tt.by != "" {
				assert.Equal(t, "2026-11-17T12:34:56Z", mappingField(b.fields["generated"], "at").Value)
				assert.Nil(t, b.fields["last_edited"])
			} else {
				assert.Nil(t, b.fields["generated"])
			}
			data, _, err := b.encode()
			require.NoError(t, err)
			assert.True(t, bytes.HasSuffix(data, []byte(tt.body)))
			changed, _ = b.fixOKF("docs/file.md", tt.rules, tt.by, tt.description)
			assert.False(t, changed)
		})
	}
}

func TestOKFFixFiles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("one.md", "# Heading\n\nBody\n")
	repo.WriteFile("two.md", goodOKF)
	repo.Run("add", "one.md", "two.md")
	var out, errout bytes.Buffer
	_, err := runFrontmatter(t.Context(), []string{"-fix", "-description", "Words"}, &out, &errout)
	require.ErrorContains(t, err, "exactly one")
	args := []string{"-fix", "-type-rule", "**=Guide", "-description", "Words", "one.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errout)
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.Contains(t, errout.String(), "-by ACTOR")
	out.Reset()
	errout.Reset()
	args = []string{"-fix", "-type-rule", "**=Guide", "-description", "Words", "-by", "human:test", "one.md"}
	n, err = runFrontmatter(t.Context(), args, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
	data, err := os.ReadFile("one.md")
	require.NoError(t, err)
	assert.True(t, bytes.HasSuffix(data, []byte("# Heading\n\nBody\n")))
	out.Reset()
	errout.Reset()
	_, err = runFrontmatter(t.Context(), args, &out, &errout)
	require.NoError(t, err)
	again, err := os.ReadFile("one.md")
	require.NoError(t, err)
	assert.Equal(t, data, again)
	for _, src := range []string{"---\ntype: 123\n---\n# H\n", "---\ntitle: [\n---\n# H\n", "---\ngenerated: bad\n---\n# H\n"} {
		repo.WriteFile("one.md", src)
		out.Reset()
		errout.Reset()
		_, err = runFrontmatter(t.Context(), args, &out, &errout)
		require.NoError(t, err)
		got, err := os.ReadFile("one.md")
		require.NoError(t, err)
		assert.Equal(t, src, string(got))
	}
}

func TestOKFCanonicalFormatting(t *testing.T) {
	tests := []struct {
		name, a, b string
		equal      bool
	}{
		{"rewrap", "A long line of prose.\n", "A long line\nof prose.\n", true},
		{"table padding", "| A | B |\n|---|---|\n| x | y |\n", "| A   | B   |\n| --- | --- |\n| x   | y   |\n", true},
		{"bullet", "* one\n* two\n", "- one\n- two\n", true},
		{"list tightness", "- one\n- two\n", "- one\n\n- two\n", false},
		{"ordered marker", "1. one\n2. two\n", "1) one\n2) two\n", true},
		{"task checked", "- [ ] task\n", "- [x] task\n", false},
		{"ordered list", "- item\n", "1. item\n", false},
		{"ordered start", "1. item\n", "2. item\n", false},
		{"autolink", "<https://a.example>\n", "<https://b.example>\n", false},
		{"table alignment", "| a |\n| :--- |\n| x |\n", "| a |\n| ---: |\n| x |\n", false},
		{"emphasis", "*hello*\n", "_hello_\n", true},
		{"crlf", "# Title\n\nBody\n", "# Title\r\n\r\nBody\r\n", true},
		{"nonbreaking space", "a\u00a0b\n", "a b\n", false},
		{"inline spacing", "hello **world**\n", "hello**world**\n", false},
		{"heading", "# One\n", "# Two\n", false},
		{"link", "[x](a)\n", "[x](b)\n", false},
		{"code indentation", "```\na\n```\n", "```\n a\n```\n", false},
		{"code span", "`a b`\n", "`a  b`\n", false},
		{"code span rewrap", "`one two`\n", "`one\ntwo`\n", true},
		{"html", "<b>x</b>\n", "<i>x</i>\n", false},
		{"hard break", "a\nb\n", "a  \nb\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.equal, canonicalHTML([]byte(tt.a)) == canonicalHTML([]byte(tt.b)))
		})
	}
}

func TestOKFChangedMatrix(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	src := strings.Replace(goodOKF, "Text here.", "[link](a)\n\n`a b`\n\n<pre class=\"sample\">a b</pre>\n\n<span title=\"a b\">text</span>\n\n```\ncode\n```", 1)
	repo.WriteFile("page.md", src)
	repo.WriteFile("old.md", goodFrontmatter)
	repo.Run("add", "page.md", "old.md")
	repo.Run("commit", "-m", "base pages")
	repo.Run("branch", "base")
	cases := []struct {
		name, content string
		stale         bool
	}{
		{"body", strings.Replace(src, "[link]", "[changed]", 1), true},
		{"backwards timestamp", strings.Replace(strings.Replace(src, "[link]", "[changed]", 1), "12:00:00Z", "11:59:59Z", 1), true},
		{"same instant different spelling", strings.Replace(strings.Replace(src, "[link]", "[changed]", 1), "12:00:00Z", "12:00:00.000Z", 1), true},
		{"same day same actor", strings.Replace(strings.Replace(src, "[link]", "[changed]", 1), "12:00:00Z", "12:01:00Z", 1), false},
		{"title", strings.Replace(src, "title: A page", "title: Other", 1), true},
		{"status", strings.Replace(src, "type: Guide\n", "type: Guide\nstatus: deprecated\n", 1), true},
		{"link", strings.Replace(src, "(a)", "(b)", 1), true},
		{"code indent", strings.Replace(src, "\ncode\n", "\n code\n", 1), true},
		{"inline code whitespace", strings.Replace(src, "`a b`", "`a  b`", 1), true},
		{"pre whitespace", strings.Replace(src, ">a b</pre>", ">a  b</pre>", 1), true},
		{"attribute whitespace", strings.Replace(src, `title="a b"`, `title="a  b"`, 1), true},
		{"crlf", strings.ReplaceAll(src, "\n", "\r\n"), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			repo.WriteFile("page.md", tt.content)
			var out, errout bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &out, &errout)
			require.NoError(t, err)
			if tt.stale {
				assert.Equal(t, 1, n, errout.String())
				assert.Contains(t, errout.String(), "content changed without a later generated.at")
			} else {
				assert.Zero(t, n, errout.String())
			}
		})
	}
	repo.WriteFile("page.md", src)
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Chmod("page.md", 0o755))
		var out, errout bytes.Buffer
		n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "page.md"}, &out, &errout)
		require.NoError(t, err)
		assert.Zero(t, n, errout.String())
		require.NoError(t, os.Chmod("page.md", 0o644))
	}
	repo.WriteFile("old.md", goodOKF)
	repo.WriteFile("new.md", goodOKF)
	repo.Run("add", "new.md")
	require.NoError(t, os.Remove("page.md"))
	var out, errout bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
}

func TestOKFSelectionAndSkills(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	for _, name := range []string{"README.md", "docs/AGENTS.md", "skills/demo/SKILL.md", "skills/demo/references/note.md", "docs/testdata/page.md", "docs/page.md"} {
		repo.WriteFile(name, "# No metadata\n")
		repo.Run("add", "-f", "--", name)
	}
	var out, errout bytes.Buffer
	n, err := runFrontmatter(t.Context(), nil, &out, &errout)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, errout.String(), "docs/page.md:")
	assert.NotContains(t, errout.String(), "references/note.md:")
	assert.FileExists(t, filepath.Join("skills", "demo", "SKILL.md"))
}

func TestOKFFixPreservesValidMetadata(t *testing.T) {
	b, err := parseFrontmatter([]byte(goodOKF))
	require.NoError(t, err)
	changed, notes := b.fixOKF("docs/page.md", frontmatterRules{{"docs/**", "Reference"}}, "human:other", "Other description")
	assert.False(t, changed)
	assert.Empty(t, notes)
	assert.Equal(t, "Guide", b.fields["type"].Value)
	assert.Equal(t, "Useful", b.fields["description"].Value)
	assert.Equal(t, "human:test", mappingField(b.fields["generated"], "by").Value)
	assert.Equal(t, "2026-11-17T12:00:00Z", mappingField(b.fields["generated"], "at").Value)
}

func TestOKFGeneratedMetadataComparison(t *testing.T) {
	base, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1)))
	require.NoError(t, err)
	cases := []struct {
		name, src  string
		meaningful bool
	}{
		{"other generated field", strings.Replace(goodOKF, "  by: human:test\n", "  source: changed\n  by: human:test\n", 1), true},
		{"actor only", strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:other\n", 1), false},
		{"timestamp only", strings.Replace(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1), "12:00:00Z", "12:01:00Z", 1), false},
		{"last edited only", strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1), false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.src
			if tt.name == "last edited only" {
				src = strings.Replace(src, "type: Guide\n", "last_edited: 2024-01-01\ntype: Guide\n", 1)
			}
			b, err := parseFrontmatter([]byte(src))
			require.NoError(t, err)
			assert.Equal(t, tt.meaningful, frontmatterMeaningful(base, b))
		})
	}
}

func TestOKFFixDropsLegacyDateWithCompleteGenerated(t *testing.T) {
	for _, generated := range []string{"generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z", "generated:\n  by: human:test"} {
		t.Run(generated, func(t *testing.T) {
			src := strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z", generated, 1)
			src = strings.Replace(src, "type: Guide\n", "type: Guide\nlast_edited: 2025-01-01\n", 1)
			b, err := parseFrontmatter([]byte(src))
			require.NoError(t, err)
			changed, _ := b.fixOKF("page.md", nil, "", "")
			assert.True(t, changed)
			assert.Nil(t, b.fields["last_edited"])
			changed, _ = b.fixOKF("page.md", nil, "", "")
			assert.False(t, changed)
		})
	}
}

func TestOKFTrimInputsAndScaffoldCRLF(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	original := "# Heading\r\n\r\nBody\r\n"
	repo.WriteFile("docs/page.md", original)
	repo.Run("add", "docs/page.md")
	var out, errout bytes.Buffer
	args := []string{"-fix", "-type-rule", "  docs/** = Guide  ", "-by", "  human:test  ", "-description", "  Useful description  ", "docs/page.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
	data, err := os.ReadFile("docs/page.md")
	require.NoError(t, err)
	assert.NotContains(t, strings.ReplaceAll(string(data), "\r\n", ""), "\n")
	assert.True(t, bytes.HasSuffix(data, []byte(original)))
	b, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Equal(t, "Guide", b.fields["type"].Value)
	assert.Equal(t, "Useful description", b.fields["description"].Value)
	assert.Equal(t, "human:test", mappingField(b.fields["generated"], "by").Value)
	for _, flag := range []string{"-by", "-description"} {
		out.Reset()
		errout.Reset()
		_, err = runFrontmatter(t.Context(), []string{"-fix", flag, "  ", "docs/page.md"}, &out, &errout)
		require.ErrorContains(t, err, "must not be empty")
	}
	var rules frontmatterRules
	require.Error(t, rules.Set("   = Guide"))
	require.Error(t, rules.Set("docs/**=  "))
}
