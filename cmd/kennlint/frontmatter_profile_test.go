package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gittest "go.kenn.io/kit/git/test"
	"go.kenn.io/kit/okf"
)

const goodOKF = "---\ntype: Guide\ntitle: A page\ndescription: Useful\ngenerated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n---\n\n# A page\n\nText here.\n"

func TestProfileRequirements(t *testing.T) {
	b, err := okf.Parse([]byte("---\ntype: Guide\n---\n"))
	require.NoError(t, err)
	assert.Empty(t, b.ConceptProblems())
	assert.ElementsMatch(t, []string{
		"title must be a non-empty string", "description must be a non-empty string",
		"generated.by must be a non-empty string",
		"generated.at must be an RFC 3339 timestamp with an offset or a YYYY-MM-DD date",
	}, profileProblems(b))
}

func TestFixTypeRules(t *testing.T) {
	doc := okf.New([]byte("Intro\n\n# Later heading\n"))
	rules := frontmatterRules{{"docs/**", "Guide"}, {"**/*.md", "Reference"}}
	_, notes := fixOKF(doc, "docs/page.md", rules, "human:editor", "Details")
	assert.Contains(t, notes["type"], "type rules disagree")
	assert.Contains(t, notes["title"], "first block")
	assert.Nil(t, doc.Field("generated"))

	rules[1].kind = "Guide"
	doc = okf.New([]byte("# ![icon](a.png) **Read** `this` <https://example.com>\n"))
	changed, notes := fixOKF(doc, "docs/page.md", rules, "human:editor", "Details")
	require.True(t, changed)
	require.Empty(t, notes)
	assert.Equal(t, "Read this https://example.com", doc.Field("title").Value)
	assert.Empty(t, profileProblems(doc))
	changed, _ = fixOKF(doc, "docs/page.md", rules, "human:other", "Replacement")
	assert.False(t, changed)
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
	args = []string{"-fix", "-type-rule", "**=Guide", "-description", "Words", "-by", "human:test", "one.md"}
	for _, src := range []string{"---\ntype: 123\n---\n# H\n", "---\ntitle: [\n---\n# H\n", "---\ngenerated: bad\n---\n# H\n", "---\nx-editor: &editor {by: human:first}\ngenerated: {<<: *editor}\n---\n# H\n"} {
		repo.WriteFile("one.md", src)
		out.Reset()
		errout.Reset()
		_, err = runFrontmatter(t.Context(), args, &out, &errout)
		require.NoError(t, err)
		got, err := os.ReadFile("one.md")
		require.NoError(t, err)
		assert.Equal(t, src, string(got))
	}
	original := "# Heading\r\n\r\nBody\r\n"
	repo.WriteFile("docs/page.md", original)
	repo.Run("add", "docs/page.md")
	out.Reset()
	errout.Reset()
	args = []string{"-fix", "-type-rule", "  docs/** = Guide  ", "-by", "  human:test  ", "-description", "  Useful description  ", "docs/page.md"}
	n, err = runFrontmatter(t.Context(), args, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
	data, err := os.ReadFile("docs/page.md")
	require.NoError(t, err)
	assert.NotContains(t, strings.ReplaceAll(string(data), "\r\n", ""), "\n")
	assert.True(t, bytes.HasSuffix(data, []byte(original)))
	b, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Equal(t, "Guide", b.Field("type").Value)
	assert.Equal(t, "Useful description", b.Field("description").Value)
	assert.Equal(t, "human:test", b.Field("generated", "by").Value)
	assert.Regexp(t, `(?m)^generated: \{by: [^,]+, at: "[0-9T:Z-]+"\}\r?$`, string(data))
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

func TestOKFCanonicalFormatting(t *testing.T) {
	tests := []struct {
		name, a, b string
		equal      bool
	}{
		{"referenced footnote content", "Claim.[^a]\n\n[^a]: A\n", "Claim.[^a]\n\n[^a]: <A>\n", false},
		{"footnote definition placement", "Claim.[^a]\n\n[^a]: A\n", "[^a]: A\n\nClaim.[^a]\n", true},
		{"rewrap", "A long line of prose.\n", "A long line\nof prose.\n", true},
		{"table padding", "| A | B |\n|---|---|\n| x | y |\n", "| A   | B   |\n| --- | --- |\n| x   | y   |\n", true},
		{"bullet", "* one\n* two\n", "- one\n- two\n", true},
		{"list tightness", "- one\n- two\n", "- one\n\n- two\n", false},
		{"task checked", "- [ ] task\n", "- [x] task\n", false},
		{"ordered list", "- item\n", "1. item\n", false},
		{"ordered start", "1. item\n", "2. item\n", false},
		{"autolink", "<https://a.example>\n", "<https://b.example>\n", false},
		{"table alignment", "| a |\n| :--- |\n| x |\n", "| a |\n| ---: |\n| x |\n", false},
		{"crlf", "```\na\nb\n```\n", "```\r\na\r\nb\r\n```\r\n", true},
		{"emphasis", "*hello*\n", "_hello_\n", true},
		{"nonbreaking space", "a\u00a0b\n", "a b\n", false},
		{"inline spacing", "hello **world**\n", "hello**world**\n", false},
		{"heading", "# One\n", "# Two\n", false},
		{"link", "[x](a)\n", "[x](b)\n", false},
		{"code indentation", "```\na\n```\n", "```\n a\n```\n", false},
		{"code span", "`a b`\n", "`a  b`\n", false},
		{"code span rewrap", "`one two`\n", "`one\ntwo`\n", true},
		{"html", "<b>x</b>\n", "<i>x</i>\n", false},
		{"pre whitespace", `<pre class="sample">a b</pre>`, `<pre class="sample">a  b</pre>`, false},
		{"attribute whitespace", `<span title="a b">text</span>`, `<span title="a  b">text</span>`, false},
		{"hard break", "a\nb\n", "a  \nb\n", false},
		{"prose spacing before hard break", "a  b  \nc\n", "a b  \nc\n", true},
		{"prose around inline tags", "Text  here. <kbd>a  b</kbd> more  prose", "Text here. <kbd>a  b</kbd> more prose", true},
		{"nested inline tags", "Text <KBD><b>a b</b> c  d</kbd>", "Text <KBD><b>a b</b> c d</kbd>", false},
		{"void inline tag", "Text <br> more  prose", "Text <br> more prose", true},
		{"prose spacing", "Text here.\n", "Text  here.\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.equal, canonicalHTML([]byte(tt.a)) == canonicalHTML([]byte(tt.b)))
		})
	}
}

func TestOKFDefaultStatusComparisonPreservesSource(t *testing.T) {
	before, err := okf.Parse([]byte(goodOKF))
	require.NoError(t, err)
	for _, status := range []string{"stable", "'  '", "''"} {
		content := strings.Replace(goodOKF, "type: Guide", "status: "+status+"\ntype: Guide", 1)
		after, err := okf.Parse([]byte(content))
		require.NoError(t, err)
		meaningful, err := frontmatterMeaningful(before, after)
		require.NoError(t, err)
		assert.False(t, meaningful)
		assert.NotNil(t, after.Field("status"))
	}
}

func TestOKFChangedMatrix(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	src := strings.Replace(goodOKF, "Text here.", `Text here. <span style="white-space:pre">**a b**</span>`, 1)
	repo.WriteFile("page.md", src)
	repo.WriteFile("old.md", "# Page without metadata\n")
	repo.Run("add", "page.md", "old.md")
	repo.Run("commit", "-m", "base pages")
	repo.Run("branch", "base")
	cases := []struct {
		name, content string
		stale         bool
	}{
		{"default status", strings.Replace(src, "type: Guide", "status: stable\ntype: Guide", 1), false},
		{"deprecated status", strings.Replace(src, "type: Guide", "status: deprecated\ntype: Guide", 1), true},
		{"body", strings.Replace(src, "Text here.", "Changed text.", 1), true},
		{"inline HTML whitespace", strings.Replace(src, "a b**", "a  b**", 1), true},
		{"backwards timestamp", strings.Replace(strings.Replace(src, "Text here.", "Changed text.", 1), "12:00:00Z", "11:59:59Z", 1), true},
		{"same instant different spelling", strings.Replace(strings.Replace(src, "Text here.", "Changed text.", 1), "12:00:00Z", "12:00:00.000Z", 1), true},
		{"later generated.at", strings.Replace(strings.Replace(src, "Text here.", "Changed text.", 1), "12:00:00Z", "12:01:00Z", 1), false},
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
	repo.WriteFile("old.md", goodOKF)
	var out, errout bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-changed", "base", "old.md"}, &out, &errout)
	require.NoError(t, err)
	assert.Zero(t, n, errout.String())
}

func TestOKFMetadataComparison(t *testing.T) {
	base, err := okf.Parse([]byte(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1)))
	require.NoError(t, err)
	cases := []struct {
		name, src  string
		meaningful bool
	}{
		{"other generated field", strings.Replace(goodOKF, "  by: human:test\n", "  source: changed\n  by: human:test\n", 1), true},
		{"actor only", strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:other\n", 1), false},
		{"timestamp only", strings.Replace(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1), "12:00:00Z", "12:01:00Z", 1), false},
		{"title", strings.Replace(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1), "title: A page", "title: Other", 1), true},
		{"status", strings.Replace(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1), "type: Guide\n", "type: Guide\nstatus: deprecated\n", 1), true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			b, err := okf.Parse([]byte(tt.src))
			require.NoError(t, err)
			changed, err := frontmatterMeaningful(base, b)
			require.NoError(t, err)
			assert.Equal(t, tt.meaningful, changed)
		})
	}
}

func TestOKFFixLeavesExistingValuesAlone(t *testing.T) {
	for _, tt := range []struct {
		name, src, missing string
		wantNote           bool
	}{
		{"invalid type", "---\ntype: 123\n---\n# H\n", "", false},
		{"invalid generated", "---\ngenerated: bad\n---\n# H\n", "", false},
		{"valid", goodOKF, "", false},
		{"by only", strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n", "generated: {by: 'human:old'}\n", 1), "at", true},
		{"at only", strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z\n", "generated: {at: '2020-01-01T00:00:00Z'}\n", 1), "by", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := okf.Parse([]byte(tt.src))
			require.NoError(t, err)
			before, err := b.Marshal()
			require.NoError(t, err)
			changed, notes := fixOKF(b, "docs/page.md", frontmatterRules{{"docs/**", "Reference"}}, "human:other", "Other description")
			assert.False(t, changed)
			if tt.wantNote {
				assert.Contains(t, fmt.Sprint(notes), "both")
				assert.Nil(t, b.Field("generated", tt.missing))
			} else {
				assert.Empty(t, notes)
			}
			after, err := b.Marshal()
			require.NoError(t, err)
			assert.Equal(t, before, after)
		})
	}
}

func TestOKFFixReportsEachGapOnce(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("page.md", "# Heading\n")
	repo.Run("add", "page.md")
	var out, errout bytes.Buffer
	_, err := runFrontmatter(t.Context(), []string{"-fix", "page.md"}, &out, &errout)
	require.NoError(t, err)
	for _, field := range []string{"type", "description", "generated.by", "generated.at"} {
		count := 0
		for line := range strings.SplitSeq(strings.TrimSpace(errout.String()), "\n") {
			if strings.Contains(line, field) {
				count++
			}
		}
		assert.Equal(t, 1, count, "%s: %s", field, errout.String())
	}
}
