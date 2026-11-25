package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
	"go.kenn.io/kit/okf"
)

const goodOKF = "---\ntype: Guide\ntitle: A page\ndescription: Useful\ngenerated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n---\n\n# A page\n\nText here.\n"

func TestProfileProblems(t *testing.T) {
	for _, tc := range []struct{ name, src, problem string }{
		{"valid", goodOKF, ""},
		{"date-only generated.at", strings.Replace(goodOKF, "2026-11-17T12:00:00Z", "'2026-11-17'", 1), ""},
		{"unknown status", strings.Replace(goodOKF, "type: Guide", "type: Guide\nstatus: future", 1), ""},
		{"missing type", strings.Replace(goodOKF, "type: Guide\n", "", 1), "type must be a non-empty string"},
		{"numeric title", strings.Replace(goodOKF, "title: A page", "title: 42", 1), "title must be a non-empty string"},
		{"blank description", strings.Replace(goodOKF, "description: Useful", "description: ' '", 1), "description must be a non-empty string"},
		{"numeric status", strings.Replace(goodOKF, "type: Guide", "type: Guide\nstatus: 42", 1), "status must be a string"},
		{"scalar generated", strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n", "generated: yes\n", 1), "generated must be a mapping"},
		{"missing actor", strings.Replace(goodOKF, "  by: human:test\n", "", 1), "generated.by must be a non-empty string"},
		{"no offset", strings.Replace(goodOKF, "2026-11-17T12:00:00Z", "'2026-11-17T12:00:00'", 1), "generated.at must be an RFC 3339 timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := okf.Parse([]byte(tc.src))
			require.NoError(t, err)
			problems := profileProblems(doc)
			if tc.problem == "" {
				assert.Empty(t, problems)
				return
			}
			require.Len(t, problems, 1)
			assert.Contains(t, problems[0], tc.problem)
		})
	}
}

func TestFixOKF(t *testing.T) {
	rules := frontmatterRules{{"docs/**", "Guide"}, {"**/*.md", "Reference"}}
	doc := okf.New([]byte("Intro\n\n# Later heading\n"))
	assert.True(t, fixOKF(doc, "docs/page.md", rules, "human:editor", "Details"))
	assert.Nil(t, doc.Field("type"), "disagreeing rules give no type")
	assert.Nil(t, doc.Field("title"), "an H1 that is not the first block is no title")
	assert.Equal(t, "human:editor", doc.Field("generated", "by").Value)

	rules[1].kind = "Guide"
	doc = okf.New([]byte("# ![icon](a.png) **Read** `this` <https://example.com>\n"))
	require.True(t, fixOKF(doc, "docs/page.md", rules, "human:editor", "Details"))
	assert.Equal(t, "Read this https://example.com", doc.Field("title").Value)
	assert.Empty(t, profileProblems(doc))
	assert.False(t, fixOKF(doc, "docs/page.md", rules, "human:other", "Replacement"), "present values stay")

	// A partial or invalid generated mapping is left for the author.
	src := "---\ntype: 123\ngenerated: {by: 'human:old', x-audit: {ids: [1, 2]}}\nx-root: {nested: [one, true]}\n---\n# H\n"
	doc, err := okf.Parse([]byte(src))
	require.NoError(t, err)
	require.True(t, fixOKF(doc, "docs/page.md", rules, "human:other", ""))
	data, err := doc.Marshal()
	require.NoError(t, err)
	assert.Equal(t, "---\ntype: 123\ngenerated: {by: 'human:old', x-audit: {ids: [1, 2]}}\nx-root: {nested: [one, true]}\ntitle: H\n---\n# H\n", string(data))
}

func TestFixFiles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("one.md", "---\ntitle: [\n---\n# H\n")
	repo.WriteFile("two.md", goodOKF)
	repo.Run("add", "one.md", "two.md")
	var out, errout bytes.Buffer
	_, err := runFrontmatter(t.Context(), []string{"-fix", "-description", "Words"}, &out, &errout)
	require.ErrorContains(t, err, "exactly one")

	_, err = runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "one.md"}, &out, &errout)
	require.NoError(t, err)
	got, err := os.ReadFile("one.md")
	require.NoError(t, err)
	assert.Equal(t, "---\ntitle: [\n---\n# H\n", string(got), "invalid YAML is never rewritten")

	original := "# Heading\r\n\r\nBody\r\n"
	repo.WriteFile("docs/page.md", original)
	repo.Run("add", "docs/page.md")
	errout.Reset()
	args := []string{"-fix", "-type-rule", "  docs/** = Guide  ", "-by", "  human:test  ", "-description", "  Useful description  ", "docs/page.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
	data, err := os.ReadFile("docs/page.md")
	require.NoError(t, err)
	assert.NotContains(t, strings.ReplaceAll(string(data), "\r\n", ""), "\n")
	assert.True(t, bytes.HasSuffix(data, []byte(original)))
	assert.Regexp(t, `(?m)^generated: \{by: 'human:test', at: "[0-9T:Z-]+"\}\r$`, string(data))
	b, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, "Useful description", b.Field("description").Value)
	assert.Empty(t, profileProblems(b))

	var rules frontmatterRules
	require.Error(t, rules.Set("   = Guide"))
	require.Error(t, rules.Set("docs/**=  "))
}

func TestCanonicalFormatting(t *testing.T) {
	for _, tt := range []struct {
		name, a, b string
		equal      bool
	}{
		{"rewrap", "A long line of prose.\n", "A long line\nof prose.\n", true},
		{"prose spacing", "Text here.\n", "Text  here.\n", true},
		{"table padding", "| A | B |\n|---|---|\n| x | y |\n", "| A   | B   |\n| --- | --- |\n| x   | y   |\n", true},
		{"bullet", "* one\n* two\n", "- one\n- two\n", true},
		{"emphasis", "*hello*\n", "_hello_\n", true},
		{"crlf", "```\na\nb\n```\n", "```\r\na\r\nb\r\n```\r\n", true},
		{"footnote placement", "Claim.[^a]\n\n[^a]: A\n", "[^a]: A\n\nClaim.[^a]\n", true},
		{"code span rewrap", "`one two`\n", "`one\ntwo`\n", true},
		{"prose spacing before hard break", "a  b  \nc\n", "a b  \nc\n", true},
		{"list tightness", "- one\n- two\n", "- one\n\n- two\n", false},
		{"task checked", "- [ ] task\n", "- [x] task\n", false},
		{"ordered list", "- item\n", "1. item\n", false},
		{"table alignment", "| a |\n| :--- |\n| x |\n", "| a |\n| ---: |\n| x |\n", false},
		{"inline spacing", "hello **world**\n", "hello**world**\n", false},
		{"heading", "# One\n", "# Two\n", false},
		{"link", "[x](a)\n", "[x](b)\n", false},
		{"code indentation", "```\na\n```\n", "```\n a\n```\n", false},
		{"code span", "`a b`\n", "`a  b`\n", false},
		{"html", "<b>x</b>\n", "<i>x</i>\n", false},
		{"hard break", "a\nb\n", "a  \nb\n", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.equal, canonicalHTML([]byte(tt.a)) == canonicalHTML([]byte(tt.b)))
		})
	}
}

func TestMetadataMeaningful(t *testing.T) {
	base, err := okf.Parse([]byte(goodOKF))
	require.NoError(t, err)
	for _, tt := range []struct {
		name, src  string
		meaningful bool
	}{
		{"actor", strings.Replace(goodOKF, "human:test", "human:other", 1), false},
		{"timestamp", strings.Replace(goodOKF, "12:00:00Z", "12:01:00Z", 1), false},
		{"verification", strings.Replace(goodOKF, "type: Guide\n", "type: Guide\nverified: {by: 'human:reviewer', at: '2026-11-18'}\n", 1), false},
		{"block to flow", strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n", "generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}\n", 1), false},
		{"other generated field", strings.Replace(goodOKF, "  by: human:test\n", "  source: changed\n  by: human:test\n", 1), true},
		{"title", strings.Replace(goodOKF, "title: A page", "title: Other", 1), true},
		{"status", strings.Replace(goodOKF, "type: Guide\n", "type: Guide\nstatus: deprecated\n", 1), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := okf.Parse([]byte(tt.src))
			require.NoError(t, err)
			meaningful, err := frontmatterMeaningful(base, b)
			require.NoError(t, err)
			assert.Equal(t, tt.meaningful, meaningful)
		})
	}
}

func TestChangedNeedsLaterTimestamp(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("page.md", goodOKF)
	repo.WriteFile("old.md", "# Page without metadata\n")
	repo.WriteFile("index.md", "---\nokf_version: \"0.3\"\n---\n# Index\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "base pages")
	repo.Run("branch", "base")
	edited := strings.Replace(goodOKF, "Text here.", "Changed text.", 1)
	for _, tt := range []struct {
		name, content string
		stale         bool
	}{
		{"rewrap", strings.Replace(goodOKF, "Text here.", "Text\nhere.", 1), false},
		{"body", edited, true},
		{"earlier generated.at", strings.Replace(edited, "12:00:00Z", "11:59:59Z", 1), true},
		{"same instant", strings.Replace(edited, "12:00:00Z", "12:00:00.000Z", 1), true},
		{"later generated.at", strings.Replace(edited, "12:00:00Z", "12:01:00Z", 1), false},
	} {
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
	// Adopting metadata and editing a reserved index need no timestamp.
	repo.WriteFile("page.md", goodOKF)
	repo.WriteFile("old.md", goodOKF)
	repo.WriteFile("index.md", "---\nokf_version: \"0.3\"\n---\n# Index\n\n- [Old](old.md)\n")
	var out, errout bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base"}, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
}

func TestReservedFiles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	index := "---\nokf_version: \"0.3\"\ntitle: Bundle\nx-custom: {nested: [one, true, 2]}\n---\n# Index\n"
	repo.WriteFile("index.md", index)
	repo.WriteFile("docs/index.md", "# Docs\n")
	repo.WriteFile("log.md", goodOKF)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n, errs.String())
	assert.Equal(t, "log.md: log.md must not have frontmatter\n", errs.String())
	for name, want := range map[string]string{"index.md": index, "docs/index.md": "# Docs\n", "log.md": goodOKF} {
		got, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, want, string(got), name)
	}
}

func TestReleaseNotesKeepNativeFormat(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const src = "# Release notes\n"
	files := []string{"release-notes.md", "docs/release-notes.md", "pkg/RELEASE-NOTES.md", "pkg/RELEASE_NOTES.md", "pkg/release_notes.md"}
	for _, file := range files {
		repo.WriteFile(file, src)
	}
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	for _, file := range files {
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, src, string(data), file)
	}
}
