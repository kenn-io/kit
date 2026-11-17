package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
	"gopkg.in/yaml.v3"
)

const skillHeader = "---\nname: example\ndescription: Example\nmetadata:\n  type: Skill\n  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"\n---\nBody text.\n"

func TestSkillProfileShapes(t *testing.T) {
	cases := []struct {
		name, src string
		valid     bool
	}{
		{"valid", skillHeader, true},
		{"status", strings.Replace(skillHeader, "---\nBody", "  status: draft\n---\nBody", 1), true},
		{"generated extra", strings.Replace(skillHeader, "    by: human:test", "    source: changed\n    by: human:test", 1), true},
		{"metadata last edited", strings.Replace(skillHeader, "  type: Skill", "  last_edited: note\n  type: Skill", 1), true},
		{"missing metadata", "---\nname: x\n---\n", false},
		{"flow metadata", "---\nmetadata: {type: Skill}\n---\n", false},
		{"scalar metadata", "---\nmetadata: x\n---\n", false},
		{"missing type", strings.Replace(skillHeader, "  type: Skill\n", "", 1), false},
		{"wrong type", strings.Replace(skillHeader, "type: Skill", "type: skill", 1), false},
		{"flow generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: {by: x, at: y}", 1), false},
		{"list generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: [x]", 1), false},
		{"scalar generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: x", 1), false},
		{"empty by", strings.Replace(skillHeader, "human:test", "''", 1), false},
		{"numeric by", strings.Replace(skillHeader, "human:test", "42", 1), false},
		{"bare at", strings.ReplaceAll(skillHeader, "\"", ""), false},
		{"offset at", strings.Replace(skillHeader, "12:00:00Z", "12:00:00+01:00", 1), false},
		{"date at", strings.Replace(skillHeader, "2026-11-17T12:00:00Z", "2026-11-17", 1), false},
		{"status invalid", strings.Replace(skillHeader, "---\nBody", "  status: retired\n---\nBody", 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(tc.src))
			require.NoError(t, err)
			assert.Equal(t, tc.valid, len(b.skillProblems()) == 0, b.skillProblems())
		})
	}
	b, err := parseFrontmatter([]byte(skillHeader))
	require.NoError(t, err)
	assertSkillNodeKinds(t, b)
	var decoded map[string]any
	header, _, ok := strings.Cut(strings.TrimPrefix(skillHeader, "---\n"), "\n---\n")
	require.True(t, ok)
	require.NoError(t, yaml.Unmarshal([]byte(header), &decoded))
	meta := decoded["metadata"].(map[string]any)
	assert.IsType(t, "", meta["type"])
	assert.IsType(t, "", meta["generated"].(map[string]any)["at"])
}

func assertSkillNodeKinds(t *testing.T, b *frontmatterBlock) {
	t.Helper()
	m := b.fields["metadata"]
	require.NotNil(t, m)
	require.Equal(t, yaml.MappingNode, m.Kind)
	assert.Zero(t, m.Style&yaml.FlowStyle)
	for i := 1; i < len(m.Content); i += 2 {
		n := m.Content[i]
		if m.Content[i-1].Value == "generated" {
			require.Equal(t, yaml.MappingNode, n.Kind)
			assert.Zero(t, n.Style&yaml.FlowStyle)
			for j := 1; j < len(n.Content); j += 2 {
				assert.Equal(t, yaml.ScalarNode, n.Content[j].Kind)
				assert.Equal(t, "!!str", n.Content[j].Tag)
			}
		} else {
			assert.Equal(t, yaml.ScalarNode, n.Kind)
			assert.Equal(t, "!!str", n.Tag)
		}
	}
}

func TestSkillSelectionAndFix(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	old := frontmatterNow
	frontmatterNow = func() time.Time { return time.Date(2026, 11, 17, 12, 0, 0, 123000000, time.UTC) }
	t.Cleanup(func() { frontmatterNow = old })
	src := "---\nname: native\ndescription: words\nother: keep # comment\n---\nBody\n"
	repo.WriteFile("skills/a/SKILL.md", src)
	repo.WriteFile("skills/a/vendor/b/SKILL.md", src)
	repo.WriteFile("skills/a/references/page.md", "bundle")
	repo.WriteFile("portable/SKILL.md", "---\nmetadata: {tags: [a, b]}\n---\n")
	repo.WriteFile("docs/page.md", goodOKF)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	args := []string{"-managed-skill", "skills/**/SKILL.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.NotContains(t, errs.String(), "fix locally:")
	out.Reset()
	errs.Reset()
	fix := append([]string{"-fix", "-by", "human:test"}, args...)
	n, err = runFrontmatter(t.Context(), fix, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Contains(t, string(data), "at: \"2026-11-17T12:00:00Z\"")
	assert.Contains(t, string(data), "# comment")
	assert.NotContains(t, string(data), "title:")
	assert.True(t, bytes.HasSuffix(data, []byte("Body\n")))
	written, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Empty(t, written.skillProblems())
	assertSkillNodeKinds(t, written)
	out.Reset()
	errs.Reset()
	_, err = runFrontmatter(t.Context(), fix, &out, &errs)
	require.NoError(t, err)
	again, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, data, again)
	nested, err := os.ReadFile("skills/a/vendor/b/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, src, string(nested))
	data, err = os.ReadFile("portable/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "---\nmetadata: {tags: [a, b]}\n---\n", string(data))
	repo.WriteFile("skills/a/SKILL.md", "no header\n")
	out.Reset()
	errs.Reset()
	n, err = runFrontmatter(t.Context(), fix, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	data, err = os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "no header\n", string(data))
	_, err = runFrontmatter(t.Context(), []string{"-legacy", "-managed-skill", "x"}, &out, &errs)
	require.Error(t, err)
}

func TestSkillChangedAndImports(t *testing.T) {
	skillHeader := strings.Replace(skillHeader, "Body text.", "Body text. `printf 'a  b'`", 1)
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.WriteFile("skills/b/SKILL.md", "---\nname: old\n---\nbody\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "base skills")
	repo.Run("branch", "base")
	args := []string{"-managed-skill", "skills/**/SKILL.md", "-changed", "base", "skills/a/SKILL.md"}
	for _, tc := range []struct {
		name, content string
		stale         bool
	}{
		{"body", strings.Replace(skillHeader, "Body text.", "Changed text.", 1), true},
		{"inline code whitespace", strings.Replace(skillHeader, "`printf 'a  b'`", "`printf 'a b'`", 1), true},
		{"timestamp", strings.Replace(strings.Replace(skillHeader, "Body text.", "Changed text.", 1), "12:00:00Z", "12:01:00Z", 1), false},
		{"same instant", strings.Replace(strings.Replace(skillHeader, "Body text.", "Changed text.", 1), "12:00:00Z", "12:00:00.000Z", 1), true},
		{"earlier instant", strings.Replace(strings.Replace(skillHeader, "Body text.", "Changed text.", 1), "12:00:00Z", "11:59:59Z", 1), true},
		{"format", strings.Replace(skillHeader, "Body text.", "Body\ntext.", 1), false},
		{"status", strings.Replace(skillHeader, "---\nBody", "  status: draft\n---\nBody", 1), true},
		{"generated extra", strings.Replace(skillHeader, "    by: human:test", "    source: changed\n    by: human:test", 1), true},
		{"metadata last edited", strings.Replace(skillHeader, "  type: Skill", "  last_edited: note\n  type: Skill", 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo.WriteFile("skills/a/SKILL.md", tc.content)
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), args, &out, &errs)
			require.NoError(t, err)
			if tc.stale {
				assert.Equal(t, 1, n)
				assert.Contains(t, errs.String(), "metadata.generated.at")
			} else {
				assert.Zero(t, n, errs.String())
			}
		})
	}
	repo.WriteFile("skills/b/SKILL.md", skillHeader)
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/**/SKILL.md", "-changed", "base", "skills/b/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
}

func TestSkillFixRefusesNullMappings(t *testing.T) {
	for _, src := range []string{
		"---\nname: x\nmetadata:\n---\nBody\n",
		"---\nmetadata:\n  type: Skill\n  generated:\n---\nBody\n",
	} {
		t.Run(src, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(src))
			require.NoError(t, err)
			assert.NotEmpty(t, b.skillProblems())
			before, _, err := b.encode()
			require.NoError(t, err)
			changed, notes := b.fixSkill("human:test")
			assert.False(t, changed)
			assert.Equal(t, "correct invalid skill metadata before fixing", notes["invalid"])
			data, _, err := b.encode()
			require.NoError(t, err)
			assert.Equal(t, before, data)
		})
	}
}

func TestSkillInvalidAndSymlink(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	invalid := strings.Replace(skillHeader, "type: Skill", "type: wrong", 1)
	repo.WriteFile("skills/a/SKILL.md", invalid)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	args := []string{"-fix", "-by", "human:test", "-managed-skill", "skills/**/SKILL.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, invalid, string(data))
	if err := os.Symlink("../a/SKILL.md", "skills/link/SKILL.md"); err != nil {
		require.NoError(t, os.MkdirAll("skills/link", 0o755))
		if err = os.Symlink("../a/SKILL.md", "skills/link/SKILL.md"); err != nil {
			t.Skip("symlinks unavailable")
		}
	}
	repo.Run("add", "skills/link/SKILL.md")
	out.Reset()
	errs.Reset()
	n, err = runFrontmatter(t.Context(), []string{"-managed-skill", "skills/link/SKILL.md", "-fix"}, &out, &errs)
	require.ErrorContains(t, err, "skills/link/SKILL.md")
	assert.Zero(t, n, errs.String())
	data, err = os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, invalid, string(data))
}

func TestSkillBundleExclusionsAndLegacy(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const invalid = "not frontmatter\n"
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	bundleFiles := []string{"skills/a/references/page.md", "skills/a/assets/page.md", "skills/a/templates/page.md", "skills/a/testdata/page.md"}
	for _, name := range bundleFiles {
		repo.WriteFile(name, invalid)
	}
	repo.WriteFile("other/SKILL.md", invalid)
	repo.Run("add", ".")
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"managed", []string{"-fix", "-by", "human:test", "-managed-skill", "skills/**/SKILL.md"}},
		{"unmatched", []string{"-fix", "-by", "human:test", "-managed-skill", "nowhere/**/SKILL.md"}},
		{"legacy", []string{"-legacy", "-fix"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skill := invalid
			if tc.name == "managed" {
				skill = skillHeader
			}
			repo.WriteFile("skills/a/SKILL.md", skill)
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), tc.args, &out, &errs)
			if tc.name == "unmatched" {
				require.ErrorContains(t, err, "nowhere/**/SKILL.md")
			} else {
				require.NoError(t, err)
				assert.Zero(t, n, errs.String())
			}
			assert.Empty(t, errs.String())
			for _, name := range append(bundleFiles, "other/SKILL.md") {
				data, err := os.ReadFile(name)
				require.NoError(t, err)
				assert.Equal(t, invalid, string(data), name)
			}
			data, err := os.ReadFile("skills/a/SKILL.md")
			require.NoError(t, err)
			assert.Equal(t, skill, string(data))
		})
	}
}

func TestSkillFlowRootFixAndQuotedTimestamp(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const src = "---\n{name: a, description: Useful}\n---\nBody\n"
	repo.WriteFile("skills/a/SKILL.md", src)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-managed-skill", "skills/**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.Contains(t, errs.String(), "frontmatter must be a block mapping")
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, src, string(data))

	bare := strings.ReplaceAll(skillHeader, "\"", "")
	b, err := parseFrontmatter([]byte(bare))
	require.NoError(t, err)
	assert.Contains(t, b.skillProblems(), `metadata.generated.at must be a quoted string, for example "2026-11-17T19:00:00Z"`)
}

func TestRootSkillDoesNotExcludeManagedSkills(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const missing = "---\nname: managed\n---\nBody\n"
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("skills/managed/SKILL.md", missing)
	repo.WriteFile("skills/managed/vendor/SKILL.md", missing)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	args := []string{"-managed-skill", "skills/**/SKILL.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.Contains(t, errs.String(), "skills/managed/SKILL.md:")
	assert.NotContains(t, errs.String(), "skills/managed/vendor/SKILL.md:")
	out.Reset()
	errs.Reset()
	n, err = runFrontmatter(t.Context(), append([]string{"-fix", "-by", "human:test"}, args...), &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	data, err := os.ReadFile("skills/managed/SKILL.md")
	require.NoError(t, err)
	b, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Empty(t, b.skillProblems())
	data, err = os.ReadFile("skills/managed/vendor/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, missing, string(data))
}

func TestManagedRootSkillSkipsResources(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const resource = "# Resource\n\nBody\n"
	const nested = "---\nname: nested\ndescription: Nested\n---\nBody\n"
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("references/example.md", resource)
	repo.WriteFile("skills/managed/SKILL.md", nested)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide", "-managed-skill", "**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	data, err := os.ReadFile("references/example.md")
	require.NoError(t, err)
	assert.Equal(t, resource, string(data))
	data, err = os.ReadFile("skills/managed/SKILL.md")
	require.NoError(t, err)
	b, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Empty(t, b.skillProblems())
}

func TestManagedSkillPatternsValidateTrackedFiles(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	_, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/**/SKILL.md", "-managed-skill", "typo/**/SKILL.md"}, &out, &errs)
	require.ErrorContains(t, err, "typo/**/SKILL.md")
}

func TestSkillFixGeneratedPair(t *testing.T) {
	for _, tc := range []struct {
		name, header, by      string
		wantPair, wantChanged bool
		wantNote              string
	}{
		{"missing pair", "metadata:\n  type: Skill\n", "human:test", true, true, ""},
		{"empty generated", "metadata:\n  type: Skill\n  generated: {}\n", "human:test", false, false, "correct invalid"},
		{"without actor", "metadata:\n  type: Skill\n", "", false, false, "supply -by ACTOR"},
		{"by only", "metadata:\n  type: Skill\n  generated:\n    by: human:old\n", "human:new", false, false, "supply both"},
		{"at only", "metadata:\n  type: Skill\n  generated:\n    at: \"2020-01-01T00:00:00Z\"\n", "human:new", false, false, "supply both"},
		{"invalid status", "metadata:\n  type: Skill\n  status: retired\n", "human:test", false, false, "correct invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte("---\n" + tc.header + "---\nBody\n"))
			require.NoError(t, err)
			before, _, err := b.encode()
			require.NoError(t, err)
			changed, notes := b.fixSkill(tc.by)
			assert.Equal(t, tc.wantChanged, changed)
			if tc.wantNote != "" {
				assert.Contains(t, fmt.Sprint(notes), tc.wantNote)
			}
			g := mappingField(b.fields["metadata"], "generated")
			if tc.wantPair {
				assert.Equal(t, "human:test", mappingField(g, "by").Value)
				_, valid := parseTimestamp(mappingField(g, "at"))
				assert.True(t, valid)
			} else {
				after, _, err := b.encode()
				require.NoError(t, err)
				assert.Equal(t, before, after)
			}
		})
	}
}

func TestSkillFixReportsGeneratedGapsOnce(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("skills/a/SKILL.md", "---\nname: a\nmetadata:\n  type: Skill\n---\nBody\n")
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	_, err := runFrontmatter(t.Context(), []string{"-fix", "-managed-skill", "skills/**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	for _, field := range []string{"metadata.generated.by", "metadata.generated.at"} {
		count := 0
		for line := range strings.SplitSeq(strings.TrimSpace(errs.String()), "\n") {
			if strings.Contains(line, field) {
				count++
			}
		}
		assert.Equal(t, 1, count, errs.String())
	}
}

func TestPortableRootResourcesStayUntouched(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const resource = "# Resource\n\nBody\n"
	repo.WriteFile("SKILL.md", "portable root\n")
	repo.WriteFile("references/example.md", resource)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.Run("add", ".")
	for _, args := range [][]string{{"-fix", "-by", "human:test", "-type-rule", "**=Guide"}, {"-fix", "-by", "human:test", "-type-rule", "**=Guide", "-managed-skill", "skills/**/SKILL.md"}} {
		var out, errs bytes.Buffer
		n, err := runFrontmatter(t.Context(), args, &out, &errs)
		require.NoError(t, err)
		assert.Zero(t, n, errs.String())
		data, err := os.ReadFile("references/example.md")
		require.NoError(t, err)
		assert.Equal(t, resource, string(data))
	}
}

func TestSkillFixWithoutActorDoesNotWritePartialMetadata(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const src = "---\nname: example\ndescription: Useful\n---\nBody\n"
	repo.WriteFile("skills/a/SKILL.md", src)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-managed-skill", "skills/**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.Contains(t, errs.String(), "-by ACTOR")
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, src, string(data))
}

func TestManagedSkillPatternMustSelectEligibleSkill(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.WriteFile("skills/a/vendor/SKILL.md", skillHeader)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	_, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/a/vendor/SKILL.md"}, &out, &errs)
	require.ErrorContains(t, err, "skills/a/vendor/SKILL.md")
	_, err = runFrontmatter(t.Context(), []string{"-managed-skill", "skills/a/SKILL.md", "-exclude", "skills/a/SKILL.md"}, &out, &errs)
	require.ErrorContains(t, err, "skills/a/SKILL.md")
}
