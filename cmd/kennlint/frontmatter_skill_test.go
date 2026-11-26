package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
	"go.kenn.io/kit/okf"
	"gopkg.in/yaml.v3"
)

const skillHeader = "---\nname: example\ndescription: Example\nmetadata:\n  type: Skill\n  status: stable\n  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"\n---\nBody text.\n"

func TestSkillProfileShapes(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"valid", skillHeader, ""},
		{"draft", strings.Replace(skillHeader, "status: stable", "status: draft", 1), ""},
		{"deprecated", strings.Replace(skillHeader, "status: stable", "status: deprecated", 1), ""},
		{"missing status", strings.Replace(skillHeader, "  status: stable\n", "", 1), "metadata.status must be draft, stable, or deprecated; -fix fills a missing status with stable"},
		{"generated extra", strings.Replace(skillHeader, "    by: human:test", "    source: changed\n    by: human:test", 1), ""},
		{"metadata last edited", strings.Replace(skillHeader, "  type: Skill", "  last_edited: note\n  type: Skill", 1), ""},
		{"missing metadata", "---\nname: x\n---\n", "metadata must be a mapping"},
		{"flow metadata", "---\nname: example\ndescription: Example\nmetadata: {type: Skill, status: stable, generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}}\n---\nBody text.\n", ""},
		{"scalar metadata", "---\nmetadata: x\n---\n", "metadata must be a mapping"},
		{"missing type", strings.Replace(skillHeader, "  type: Skill\n", "", 1), "metadata.type must be the string Skill"},
		{"wrong type", strings.Replace(skillHeader, "type: Skill", "type: skill", 1), "metadata.type must be the string Skill"},
		{"flow generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}", 1), ""},
		{"scalar generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: x", 1), "metadata.generated must be a mapping"},
		{"bare at", strings.ReplaceAll(skillHeader, "\"", ""), ""},
		{"status whitespace", strings.Replace(skillHeader, "status: stable", "status: '  '", 1), "metadata.status must be draft, stable, or deprecated; -fix fills a missing status with stable"},
		{"status unknown", strings.Replace(skillHeader, "status: stable", "status: retired", 1), "metadata.status must be draft, stable, or deprecated; -fix fills a missing status with stable"},
		{"status capitalized", strings.Replace(skillHeader, "status: stable", "status: Stable", 1), "metadata.status must be draft, stable, or deprecated; -fix fills a missing status with stable"},
		{"status list", strings.Replace(skillHeader, "status: stable", "status: [stable]", 1), "metadata.status must be draft, stable, or deprecated; -fix fills a missing status with stable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := okf.Parse([]byte(tc.src))
			require.NoError(t, err)
			problems := skillProblems(b)
			if tc.want == "" {
				assert.Empty(t, problems)
			} else {
				assert.Equal(t, []string{tc.want}, problems)
			}
		})
	}
	b, err := okf.Parse([]byte(skillHeader))
	require.NoError(t, err)
	assertSkillNodeKinds(t, b, false)
}

func assertSkillNodeKinds(t *testing.T, b *okf.Document, flow bool) {
	t.Helper()
	m := b.Field("metadata")
	require.NotNil(t, m)
	require.Equal(t, yaml.MappingNode, m.Kind)
	assert.Zero(t, m.Style&yaml.FlowStyle)
	for i := 1; i < len(m.Content); i += 2 {
		n := m.Content[i]
		if m.Content[i-1].Value == "generated" {
			require.Equal(t, yaml.MappingNode, n.Kind)
			assert.Equal(t, flow, n.Style&yaml.FlowStyle != 0)
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
	for _, name := range []string{"references", "assets", "templates", "testdata"} {
		repo.WriteFile("skills/a/"+name+"/page.md", "bundle")
	}
	repo.WriteFile("portable/SKILL.md", "---\nmetadata: {tags: [a, b]}\n---\n")
	repo.WriteFile("docs/page.md", goodOKF)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	args := []string{"-fix", "-managed-skill", "skills/**/SKILL.md"}
	n, err := runFrontmatter(t.Context(), args, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	assert.Contains(t, errs.String(), "-by ACTOR")
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, src, string(data), "no partial profile without -by")
	errs.Reset()
	fix := append([]string{"-by", "human:test"}, args...)
	n, err = runFrontmatter(t.Context(), fix, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	data, err = os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^  generated: \{by: [^,]+, at: "2026-11-17T12:00:00Z"\}$`, string(data))
	assert.Contains(t, string(data), "# comment")
	assert.NotContains(t, string(data), "title:")
	assert.True(t, bytes.HasSuffix(data, []byte("Body\n")))
	written, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Empty(t, skillProblems(written))
	assertSkillNodeKinds(t, written, true)
	_, err = runFrontmatter(t.Context(), fix, &out, &errs)
	require.NoError(t, err)
	again, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, data, again)
	for _, name := range []string{"references", "assets", "templates", "testdata"} {
		resource, err := os.ReadFile("skills/a/" + name + "/page.md")
		require.NoError(t, err)
		assert.Equal(t, "bundle", string(resource), name)
	}
	nested, err := os.ReadFile("skills/a/vendor/b/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, src, string(nested))
	data, err = os.ReadFile("portable/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "---\nmetadata: {tags: [a, b]}\n---\n", string(data))
	repo.WriteFile("skills/a/SKILL.md", "no header\n")
	errs.Reset()
	n, err = runFrontmatter(t.Context(), fix, &out, &errs)
	require.NoError(t, err)
	assert.Positive(t, n)
	data, err = os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, "no header\n", string(data))
	for _, bad := range [][]string{
		{"-managed-skill", "skills/**/SKILL.md", "-managed-skill", "typo/**/SKILL.md"},
		{"-managed-skill", "skills/a/vendor/b/SKILL.md"},
		{"-managed-skill", "skills/a/SKILL.md", "-exclude", "skills/a/SKILL.md"},
	} {
		_, err = runFrontmatter(t.Context(), bad, &out, &errs)
		require.ErrorContains(t, err, bad[len(bad)-1])
	}
}

func TestSkillChangedAndImports(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	src := strings.Replace(skillHeader, "Body text.", `Body text. <span style="white-space:pre">a b</span>`, 1)
	repo.WriteFile("skills/a/SKILL.md", src)
	repo.WriteFile("skills/b/SKILL.md", "---\nname: old\n---\nbody\n")
	repo.Run("add", ".")
	repo.Run("commit", "-m", "base skills")
	repo.Run("branch", "base")
	args := []string{"-managed-skill", "skills/**/SKILL.md", "-changed", "base", "skills/a/SKILL.md"}
	for _, tc := range []struct {
		name, content string
		stale         bool
	}{
		{"body", strings.Replace(src, "Body text.", "Changed text.", 1), true},
		{"inline HTML whitespace", strings.Replace(src, ">a b</span>", ">a  b</span>", 1), true},
		{"timestamp", strings.Replace(strings.Replace(src, "Body text.", "Changed text.", 1), "12:00:00Z", "12:01:00Z", 1), false},
		{"status", strings.Replace(src, "status: stable", "status: draft", 1), true},
		{"generated extra", strings.Replace(src, "    by: human:test", "    source: changed\n    by: human:test", 1), true},
		{"actor", strings.Replace(src, "human:test", "human:other", 1), false},
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

func TestSkillChangedStatus(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const path = "skills/a/SKILL.md"
	// "" is a legacy skill without status, which OKF reads as stable.
	withStatus := func(status string) string {
		if status == "" {
			return strings.Replace(skillHeader, "  status: stable\n", "", 1)
		}
		return strings.Replace(skillHeader, "status: stable", "status: "+status, 1)
	}
	const stale, invalid = "metadata.generated.at", "metadata.status must be draft, stable, or deprecated"
	for _, tc := range []struct {
		before, after, want string
	}{
		// Writing out the default is not a meaningful edit, so backfilling a
		// legacy skill needs no new timestamp.
		{"", "stable", ""},
		{"null", "stable", ""},
		{"''", "stable", ""},
		{"[]", "stable", ""},
		{"stable", "draft", stale},
		{"draft", "stable", stale},
		{"stable", "deprecated", stale},
		{"stable", "", invalid},
		{"stable", "null", invalid},
		{"stable", "experimental", invalid},
	} {
		t.Run(tc.before+"-to-"+tc.after, func(t *testing.T) {
			repo.WriteFile(path, withStatus(tc.before))
			repo.Run("add", path)
			repo.Run("commit", "--allow-empty", "-m", "base skill status")
			repo.WriteFile(path, withStatus(tc.after))
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-changed", "HEAD", "-managed-skill", path, path}, &out, &errs)
			require.NoError(t, err)
			if tc.want == "" {
				assert.Zero(t, n, errs.String())
				return
			}
			assert.Contains(t, errs.String(), tc.want)
			// An unknown value is both invalid and, unlike stable, a real
			// change from the base, so it also needs a later timestamp.
			if tc.after == "experimental" {
				assert.Equal(t, 2, n)
				assert.Contains(t, errs.String(), stale)
			} else {
				assert.Equal(t, 1, n)
			}
		})
	}
}

func TestSkillFixFlowAndSymlinkedSkills(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const flow = "---\n{name: a, description: Useful}\n---\nBody\n"
	repo.WriteFile("skills/a/SKILL.md", flow)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-managed-skill", "skills/**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	require.NoError(t, os.MkdirAll("skills/link", 0o755))
	if err := os.Symlink("../a/SKILL.md", "skills/link/SKILL.md"); err != nil {
		t.Skip("symlinks unavailable")
	}
	repo.Run("add", "skills/link/SKILL.md")
	_, err = runFrontmatter(t.Context(), []string{"-managed-skill", "skills/link/SKILL.md", "-fix", "-by", "human:test"}, &out, &errs)
	require.ErrorContains(t, err, "skills/link/SKILL.md")
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	b, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Empty(t, skillProblems(b))
	assert.Equal(t, "a", b.Field("name").Value)
}

func TestRootSkillBundleAndManagedSkills(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const resource = "# Resource\n\nBody\n"
	const missing = "---\nname: managed\n---\nBody\n"
	repo.WriteFile("SKILL.md", missing)
	repo.WriteFile("references/example.md", resource)
	repo.WriteFile("skills/managed/SKILL.md", missing)
	repo.WriteFile("skills/managed/vendor/SKILL.md", missing)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	// A root skill owns every descendant, including nested skill templates.
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide", "-managed-skill", "**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	for _, name := range []string{"SKILL.md"} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		b, err := okf.Parse(data)
		require.NoError(t, err)
		assert.Empty(t, skillProblems(b), name)
	}
	for name, want := range map[string]string{"references/example.md": resource, "skills/managed/vendor/SKILL.md": missing, "skills/managed/SKILL.md": missing} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, want, string(data), name)
	}
}

func TestSkillFixFillsEmptyGeneratedMapping(t *testing.T) {
	b, err := okf.Parse([]byte("---\nmetadata:\n  type: Skill\n  generated: {}\n---\nBody\n"))
	require.NoError(t, err)
	changed, _ := fixSkill(b, "codex")
	require.True(t, changed)
	assert.Empty(t, skillProblems(b))
	assertSkillNodeKinds(t, b, true)
}

func TestSkillFixFillsStatusAfterType(t *testing.T) {
	legacy := strings.Replace(skillHeader, "  status: stable\n", "", 1)
	for name, tc := range map[string]struct{ src, want string }{
		"block": {legacy, skillHeader},
		"flow": {
			"---\nname: example\ndescription: Example\nmetadata: {type: Skill, generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}}\n---\nBody text.\n",
			"---\nname: example\ndescription: Example\nmetadata: {type: Skill, status: stable, generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}}\n---\nBody text.\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := okf.Parse([]byte(tc.src))
			require.NoError(t, err)
			changed, _ := fixSkill(b, "human:other")
			require.True(t, changed)
			assert.Empty(t, skillProblems(b))
			out, err := b.Marshal()
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(out))
		})
	}
	b, err := okf.Parse([]byte(strings.Replace(skillHeader, "status: stable", "status: retired", 1)))
	require.NoError(t, err)
	changed, _ := fixSkill(b, "human:other")
	assert.False(t, changed, "a present status is never rewritten")
}

func TestSkillFixRefusesIncompleteProfile(t *testing.T) {
	for _, tc := range []struct{ name, header, by, note string }{
		{"null metadata", "name: x\nmetadata:\n", "human:test", ""},
		{"null generated", "metadata:\n  type: Skill\n  generated:\n", "human:test", ""},
		{"by only", "metadata:\n  type: Skill\n  generated:\n    by: human:old\n", "human:new", "supply both"},
		{"at only", "metadata:\n  type: Skill\n  generated:\n    at: \"2020-01-01T00:00:00Z\"\n", "human:new", "supply both"},
		{"wrong type", "metadata:\n  type: wrong\n", "human:test", ""},
		{"invalid status", "metadata:\n  type: Skill\n  status: [retired]\n", "human:test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := okf.Parse([]byte("---\n" + tc.header + "---\nBody\n"))
			require.NoError(t, err)
			changed, notes := fixSkill(b, tc.by)
			assert.False(t, changed)
			if tc.note != "" {
				assert.Contains(t, fmt.Sprint(notes), tc.note)
			}
		})
	}
}

func TestManagedSkillPatternOutsideSelection(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("skills/a/SKILL.md", skillHeader)
	repo.WriteFile("docs/page.md", goodOKF)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-managed-skill", "skills/**/SKILL.md", "docs"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	_, err = runFrontmatter(t.Context(), []string{"-managed-skill", "missing/**/SKILL.md", "docs"}, &out, &errs)
	require.ErrorContains(t, err, "matches no tracked SKILL.md")
}

func TestFrontmatterDocumentSymlinkSkillExclusions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const skill = "---\nname: unlisted\ndescription: Example\n---\nBody\n"
	const resource = "# Resource\n\nBody\n"
	repo.WriteFile("skills/a/SKILL.md", skill)
	external := filepath.Join(t.TempDir(), "SKILL.md")
	require.NoError(t, os.WriteFile(external, []byte(strings.Replace(skill, "Body", "# Skill", 1)), 0o600))
	repo.WriteFile("skills/a/references/page.md", resource)
	ordinary := strings.Replace(goodOKF, "type: Guide\n", "", 1)
	repo.WriteFile("other/page.md", ordinary)
	repo.WriteFile("other/log.md", resource)
	repo.WriteFile("other/index.md", ordinary)
	require.NoError(t, os.MkdirAll("docs", 0o755))
	for name, target := range map[string]string{
		"docs/skill.md":    "../skills/a/SKILL.md",
		"docs/resource.md": "../skills/a/references/page.md",
		"docs/document.md": "../other/page.md",
		"docs/journal.md":  "../other/log.md",
		"docs/contents.md": "../other/index.md",
		"docs/external.md": external,
	} {
		require.NoError(t, os.Symlink(target, name))
	}
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide", "docs"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	for name, want := range map[string]string{"other/log.md": resource, "other/index.md": ordinary, "skills/a/SKILL.md": skill, "skills/a/references/page.md": resource, external: strings.Replace(skill, "Body", "# Skill", 1)} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, want, string(data), name)
	}
	data, err := os.ReadFile("other/page.md")
	require.NoError(t, err)
	assert.Contains(t, string(data), "type: Guide")
}

func TestSkillV03TimestampsAndExtensions(t *testing.T) {
	for _, timestamp := range []string{"2026-11-17", "2026-11-17T13:00:00+01:00"} {
		b, err := okf.Parse([]byte(strings.ReplaceAll(skillHeader, "2026-11-17T12:00:00Z", timestamp)))
		require.NoError(t, err)
		assert.Empty(t, skillProblems(b))
	}
	src := "---\nname: example\ndescription: Useful\nx-root: {nested: [one, true, 2]}\nmetadata:\n  x-sync: {targets: [one, two]}\n  generated:\n    x-audit: [original, true]\n---\n# Skill\n"
	b, err := okf.Parse([]byte(src))
	require.NoError(t, err)
	var before map[string]any
	require.NoError(t, b.Metadata.Decode(&before))
	changed, _ := fixSkill(b, "human:test")
	require.True(t, changed)
	data, err := b.Marshal()
	require.NoError(t, err)
	after, err := okf.Parse(data)
	require.NoError(t, err)
	var values map[string]any
	require.NoError(t, after.Metadata.Decode(&values))
	assert.Equal(t, before["x-root"], values["x-root"])
	m := before["metadata"].(map[string]any)
	v := values["metadata"].(map[string]any)
	assert.Equal(t, m["x-sync"], v["x-sync"])
	assert.Equal(t, m["generated"].(map[string]any)["x-audit"], v["generated"].(map[string]any)["x-audit"])
	assert.Empty(t, skillProblems(after))
}

func TestSkillReviewLiveRoot(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("docs/page.md", "# No metadata\n")
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	require.NoError(t, os.Remove("SKILL.md"))
	errs.Reset()
	n, err := runFrontmatter(t.Context(), nil, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a deleted skill cannot suppress documentation checks")
	assert.Contains(t, errs.String(), "docs/page.md")
}

func TestSkillReviewNativeTimestamps(t *testing.T) {
	for _, timestamp := range []string{"2026-11-17", "2026-11-17T12:00:00Z", "2026-11-17T13:00:00+01:00"} {
		b, err := okf.Parse([]byte(strings.ReplaceAll(strings.ReplaceAll(skillHeader, "2026-11-17T12:00:00Z", timestamp), "\"", "")))
		require.NoError(t, err)
		assert.Empty(t, skillProblems(b))
	}
}

func TestSkillReviewAliasFixIsolation(t *testing.T) {
	src := "---\nname: example\ndescription: Example\nx-defaults: &defaults\n  x-sync: [one, two]\nmetadata: *defaults\n---\n# Skill\n"
	b, err := okf.Parse([]byte(src))
	require.NoError(t, err)
	changed, _ := fixSkill(b, "human:test")
	require.True(t, changed)
	data, err := b.Marshal()
	require.NoError(t, err)
	after, err := okf.Parse(data)
	require.NoError(t, err)
	assert.Nil(t, mappingField(after.Field("x-defaults"), "type"))
	assert.Nil(t, mappingField(after.Field("x-defaults"), "generated"))
	assert.Empty(t, skillProblems(after))
}

func TestSkillReviewExternalResourceSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	external := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(external, "references"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(external, "SKILL.md"), []byte(skillHeader), 0o600))
	const src = "# Resource\nPortable body\n"
	target := filepath.Join(external, "references", "page.md")
	require.NoError(t, os.WriteFile(target, []byte(src), 0o600))
	require.NoError(t, os.Symlink(target, "alias.md"))
	require.NoError(t, os.Symlink("alias.md", "chain.md"))
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-description", "Description", "-type-rule", "**=Guide", "chain.md"}, &out, &errs)
	// No eligible document remains, so -description cannot apply.
	require.ErrorContains(t, err, "exactly one selected file")
	assert.Zero(t, n)
	n, err = runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, src, string(got))
}

func TestSkillReviewUnlistedRootScope(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", skillHeader)
	repo.WriteFile("docs/page.md", "# Missing metadata\n")
	const resource = "# Portable resource\n"
	repo.WriteFile("references/page.md", resource)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	_, err := runFrontmatter(t.Context(), nil, &out, &errs)
	require.ErrorContains(t, err, "root SKILL.md")
	n, err := runFrontmatter(t.Context(), []string{"docs/page.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n, errs.String())
	assert.Contains(t, errs.String(), "docs/page.md")
	_, err = runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide", "references/page.md"}, &out, &errs)
	require.ErrorContains(t, err, "read-only")
	got, err := os.ReadFile("references/page.md")
	require.NoError(t, err)
	assert.Equal(t, resource, string(got))
}

func TestSkillReviewAnchoredSnapshots(t *testing.T) {
	for _, header := range []string{
		"metadata: &original {x-sync: [one, true]}\nx-snapshot: *original\n",
		"metadata:\n  type: Skill\n  generated: &original {x-audit: [one, true]}\n  x-snapshot: *original\n",
	} {
		b, err := okf.Parse([]byte("---\nname: example\ndescription: Example\n" + header + "---\nBody\n"))
		require.NoError(t, err)
		var before map[string]any
		require.NoError(t, b.Metadata.Decode(&before))
		changed, _ := fixSkill(b, "human:test")
		require.True(t, changed)
		data, err := b.Marshal()
		require.NoError(t, err)
		after, err := okf.Parse(data)
		require.NoError(t, err)
		var values map[string]any
		require.NoError(t, after.Metadata.Decode(&values))
		assert.Equal(t, before["x-snapshot"], values["x-snapshot"])
		if metadata := before["metadata"].(map[string]any); metadata["x-snapshot"] != nil {
			assert.Equal(t, metadata["x-snapshot"], values["metadata"].(map[string]any)["x-snapshot"])
		}
		assert.Empty(t, skillProblems(after))
	}
}

func TestSkillReviewWholeRootSelections(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	repo.WriteFile("SKILL.md", skillHeader)
	const resource = "# Portable resource\n"
	for _, file := range []string{"references/x.md", "assets/t.md", "docs/page.md"} {
		repo.WriteFile(file, resource)
	}
	repo.Run("add", ".")
	for _, selection := range []string{".", "./", repo.Root} {
		t.Run(selection, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				args []string
			}{
				{"unlisted", []string{selection}},
				{"fix", []string{"-fix", "-by", "human:test", selection}},
				{"listed", []string{"-managed-skill", "SKILL.md", selection}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var out, errs bytes.Buffer
					n, err := runFrontmatter(t.Context(), tc.args, &out, &errs)
					if tc.name == "listed" {
						require.NoError(t, err)
					} else {
						require.ErrorContains(t, err, "root SKILL.md")
					}
					assert.Zero(t, n, errs.String())
					assert.NotContains(t, errs.String(), "references/x.md")
					assert.NotContains(t, errs.String(), "assets/t.md")
				})
			}
		})
	}
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"docs/page.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "explicit ordinary document paths remain readable")
	assert.Contains(t, errs.String(), "docs/page.md")
	for _, file := range []string{"references/x.md", "assets/t.md", "docs/page.md"} {
		got, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.Equal(t, resource, string(got), file)
	}
}

func TestFrontmatterReviewAncestorSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	for _, boundary := range []string{"external", "skill", "excluded", "ordinary"} {
		t.Run(boundary, func(t *testing.T) {
			isolateFrontmatterGit(t)
			repo := gittest.NewRepoWithCommit(t)
			t.Chdir(repo.Root)
			repo.WriteFile("docs/import/page.md", "# Tracked page\n")
			const resource = "# Protected page\nPortable body\n"
			targetDir := filepath.Join(repo.Root, "other")
			switch boundary {
			case "external":
				targetDir = t.TempDir()
			case "skill":
				repo.WriteFile("skills/a/SKILL.md", skillHeader)
				targetDir = filepath.Join(repo.Root, "skills", "a", "references")
			case "excluded":
				targetDir = filepath.Join(repo.Root, "excluded")
			}
			require.NoError(t, os.MkdirAll(targetDir, 0o700))
			target := filepath.Join(targetDir, "page.md")
			require.NoError(t, os.WriteFile(target, []byte(resource), 0o600))
			repo.Run("add", ".")
			require.NoError(t, os.RemoveAll("docs/import"))
			require.NoError(t, os.Symlink(targetDir, "docs/import"))
			var out, errs bytes.Buffer
			n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-description", "Description", "-type-rule", "**=Guide", "-exclude", "excluded/**", "docs/import/page.md"}, &out, &errs)
			if boundary == "ordinary" {
				require.NoError(t, err)
				assert.Zero(t, n, errs.String())
			} else {
				require.ErrorContains(t, err, "exactly one selected file")
			}
			got, readErr := os.ReadFile(target)
			require.NoError(t, readErr)
			if boundary == "ordinary" {
				assert.Contains(t, string(got), "generated:")
			} else {
				assert.Equal(t, resource, string(got), boundary)
			}
		})
	}
}

func TestSkillReviewAncestorSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privileges on Windows")
	}
	const unmanaged = "---\nname: example\ndescription: Example\n---\nBody text.\n"
	for _, boundary := range []string{"external", "internal"} {
		t.Run(boundary, func(t *testing.T) {
			isolateFrontmatterGit(t)
			repo := gittest.NewRepoWithCommit(t)
			t.Chdir(repo.Root)
			repo.WriteFile("skills/own/SKILL.md", unmanaged)
			repo.WriteFile("skills/imported/SKILL.md", unmanaged)
			targetDir := t.TempDir()
			if boundary == "internal" {
				targetDir = filepath.Join(repo.Root, "vendored", "imported")
			}
			require.NoError(t, os.MkdirAll(targetDir, 0o700))
			target := filepath.Join(targetDir, "SKILL.md")
			require.NoError(t, os.WriteFile(target, []byte(unmanaged), 0o600))
			repo.Run("add", ".")
			require.NoError(t, os.RemoveAll("skills/imported"))
			require.NoError(t, os.Symlink(targetDir, "skills/imported"))
			var out, errs bytes.Buffer
			_, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-managed-skill", "skills/**/SKILL.md"}, &out, &errs)
			require.NoError(t, err)
			got, err := os.ReadFile(target)
			require.NoError(t, err)
			assert.Equal(t, unmanaged, string(got), "aliased skill is an import")
			got, err = os.ReadFile("skills/own/SKILL.md")
			require.NoError(t, err)
			assert.Contains(t, string(got), "generated:", "directly owned skill stays opted in")
		})
	}
}
