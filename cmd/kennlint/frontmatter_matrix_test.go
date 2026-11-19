package main

import (
	"bytes"
	"fmt"
	"os"
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
		{"type: Guide\n", "type: Guide\nlast_edited: 2020-01-01\n", "last_edited is replaced by generated.at; remove it"},
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
		{"2026-11-17T12:00:00Z", "2026-11-17T12:00:00", "generated.at"},
	}
	for i, tt := range fields {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, tt.old, tt.invalid, 1)))
			require.NoError(t, err)
			assert.Contains(t, strings.Join(b.okfProblems(), "\n"), tt.problem)
		})
	}
	for _, src := range []string{goodOKF, strings.ReplaceAll(goodOKF, "\n", "\r\n"), strings.Replace(goodOKF, "2026-11-17T12:00:00Z", "'2026-11-17T12:00:00.123Z'", 1), strings.Replace(goodOKF, "generated:\n  by: human:test\n  at: 2026-11-17T12:00:00Z", "generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}", 1)} {
		b, err := parseFrontmatter([]byte(src))
		require.NoError(t, err)
		assert.Empty(t, b.okfProblems())
	}
	b, err := parseFrontmatter([]byte("---\ntype: Guide\ntitle: A\ndescription: B\ngenerated: nope\n---\n"))
	require.NoError(t, err)
	assert.Contains(t, b.okfProblems(), "generated must be a mapping")
	statuses := []struct {
		name, field string
		valid       bool
	}{
		{"omitted", "", true},
		{"stable", "status: stable\n", true},
		{"draft", "status: draft\n", true},
		{"deprecated", "status: deprecated\n", true},
		{"unknown", "status: retired\n", true},
		{"empty", "status: ''\n", false},
		{"number", "status: 123\n", false},
		{"mapping", "status: {value: draft}\n", false},
	}
	for _, tt := range statuses {
		t.Run("status "+tt.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "type: Guide\n", tt.field, 1)))
			require.NoError(t, err)
			changed, _ := b.fixOKF("page.md", frontmatterRules{{"**", "Guide"}}, "", "")
			require.True(t, changed)
			if tt.valid {
				assert.Empty(t, b.okfProblems())
			} else {
				assert.Contains(t, b.okfProblems(), "status must be a non-empty string")
			}
			if tt.field != "" {
				data, _, err := b.encode()
				require.NoError(t, err)
				assert.Contains(t, string(data), tt.field)
			}
		})
	}
}

func TestOKFFixMatrix(t *testing.T) {
	old := frontmatterNow
	frontmatterNow = func() time.Time { return time.Date(2026, 11, 17, 12, 34, 56, 123000000, time.UTC) }
	t.Cleanup(func() { frontmatterNow = old })
	cases := []struct {
		name                                                 string
		rules                                                frontmatterRules
		body, by, description, wantType, wantNote, wantTitle string
		wantGenerated                                        bool
	}{
		{"autolink", frontmatterRules{{"docs/**", "Guide"}}, "# Connect to <https://example.com>\n", "human:test", "Useful", "Guide", "", "Connect to https://example.com", true},
		{"heading image", frontmatterRules{{"docs/**", "Guide"}}, "# ![img](a.png) Logo\n", "human:test", "Useful", "Guide", "", "Logo", true},
		{"formatted heading", frontmatterRules{{"docs/**", "Guide"}}, "# **Hello** world\n", "human:test", "Useful", "Guide", "", "Hello world", true},
		{"agree", frontmatterRules{{"docs/**", "Guide"}, {"**/*.md", "Guide"}}, "# Hello\n", "human:test", "Useful", "Guide", "", "", true},
		{"ambiguous", frontmatterRules{{"docs/**", "Guide"}, {"**/*.md", "Reference"}}, "# Hello\n", "human:test", "Useful", "", "type rules disagree", "", false},
		{"no rule", nil, "# Hello\n", "human:test", "Useful", "", "add type:", "", false},
		{"no heading", frontmatterRules{{"docs/**", "Guide"}}, "intro\n\n# Hello\n", "human:test", "Useful", "Guide", "first block", "", false},
		{"no actor", frontmatterRules{{"docs/**", "Guide"}}, "# Hello\n", "", "Useful", "Guide", "-by ACTOR", "", false},
		{"missing description", frontmatterRules{{"docs/**", "Guide"}}, "# Hello\n", "human:first", "", "Guide", "supply -description", "", false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte("---\n{}\n---\n" + tt.body))
			require.NoError(t, err)
			changed, notes := b.fixOKF("docs/file.md", tt.rules, tt.by, tt.description)
			require.True(t, changed)
			if tt.wantTitle != "" {
				require.NotNil(t, b.fields["title"])
				assert.Equal(t, tt.wantTitle, b.fields["title"].Value)
			}
			if tt.wantType != "" {
				require.NotNil(t, b.fields["type"])
				assert.Equal(t, tt.wantType, b.fields["type"].Value)
			}
			if tt.wantNote != "" {
				assert.Contains(t, fmt.Sprint(notes), tt.wantNote)
			}
			if tt.wantGenerated {
				assert.Equal(t, "2026-11-17T12:34:56Z", mappingField(b.fields["generated"], "at").Value)
			} else {
				assert.Nil(t, b.fields["generated"])
			}
			data, _, err := b.encode()
			require.NoError(t, err)
			assert.True(t, bytes.HasSuffix(data, []byte(tt.body)))
			changed, _ = b.fixOKF("docs/file.md", tt.rules, tt.by, tt.description)
			assert.False(t, changed)
			if tt.description == "" && tt.by != "" {
				changed, _ = b.fixOKF("docs/file.md", tt.rules, "human:second", "Details")
				require.True(t, changed)
				g := b.fields["generated"]
				require.NotNil(t, g)
				require.NotNil(t, mappingField(g, "by"))
				require.NotNil(t, mappingField(g, "at"))
				assert.Equal(t, "human:second", mappingField(g, "by").Value)
				assert.Equal(t, "2026-11-17T12:34:56Z", mappingField(g, "at").Value)
			}
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
	args = []string{"-fix", "-type-rule", "**=Guide", "-description", "Words", "-by", "human:test", "one.md"}
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
	b, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Equal(t, "Guide", b.fields["type"].Value)
	assert.Equal(t, "Useful description", b.fields["description"].Value)
	assert.Equal(t, "human:test", mappingField(b.fields["generated"], "by").Value)
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
		{"prose spacing", "Text here.\n", "Text  here.\n", true},
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
	src := strings.Replace(goodOKF, "Text here.", `Text here. <span style="white-space:pre">**a b**</span>`, 1)
	repo.WriteFile("page.md", src)
	repo.WriteFile("old.md", goodFrontmatter)
	repo.Run("add", "page.md", "old.md")
	repo.Run("commit", "-m", "base pages")
	repo.Run("branch", "base")
	cases := []struct {
		name, content string
		stale         bool
	}{
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
	base, err := parseFrontmatter([]byte(strings.Replace(goodOKF, "  by: human:test\n", "  source: original\n  by: human:test\n", 1)))
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
			b, err := parseFrontmatter([]byte(tt.src))
			require.NoError(t, err)
			assert.Equal(t, tt.meaningful, frontmatterMeaningful(base, b))
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
			b, err := parseFrontmatter([]byte(tt.src))
			require.NoError(t, err)
			before, _, err := b.encode()
			require.NoError(t, err)
			changed, notes := b.fixOKF("docs/page.md", frontmatterRules{{"docs/**", "Reference"}}, "human:other", "Other description")
			assert.False(t, changed)
			if tt.wantNote {
				assert.Contains(t, fmt.Sprint(notes), "both")
				assert.Nil(t, mappingField(b.fields["generated"], tt.missing))
			} else {
				assert.Empty(t, notes)
			}
			after, _, err := b.encode()
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

func TestOKFFixLegacyDateOnCompletion(t *testing.T) {
	src := "---\ntitle: Title\nlast_edited: 2025-01-01\n---\n# Title\n"
	b, err := parseFrontmatter([]byte(src))
	require.NoError(t, err)
	changed, _ := b.fixOKF("page.md", nil, "human:first", "")
	assert.False(t, changed)
	assert.NotNil(t, b.fields["last_edited"])
	changed, _ = b.fixOKF("page.md", frontmatterRules{{"**", "Guide"}}, "human:second", "Details")
	require.True(t, changed)
	assert.Nil(t, b.fields["last_edited"])
	assert.Empty(t, b.okfProblems())
	data, _, err := b.encode()
	require.NoError(t, err)
	assert.NotContains(t, string(data), "last_edited")
}
