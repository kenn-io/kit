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
		name, src, want string
	}{
		{"valid", skillHeader, ""},
		{"status", strings.Replace(skillHeader, "---\nBody", "  status: draft\n---\nBody", 1), ""},
		{"generated extra", strings.Replace(skillHeader, "    by: human:test", "    source: changed\n    by: human:test", 1), ""},
		{"metadata last edited", strings.Replace(skillHeader, "  type: Skill", "  last_edited: note\n  type: Skill", 1), ""},
		{"missing metadata", "---\nname: x\n---\n", "metadata must be a block mapping"},
		{"flow metadata", "---\nname: example\ndescription: Example\nmetadata: {type: Skill, generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}}\n---\nBody text.\n", "metadata must be a block mapping"},
		{"scalar metadata", "---\nmetadata: x\n---\n", "metadata must be a block mapping"},
		{"missing type", strings.Replace(skillHeader, "  type: Skill\n", "", 1), "metadata.type must be the string Skill"},
		{"wrong type", strings.Replace(skillHeader, "type: Skill", "type: skill", 1), "metadata.type must be the string Skill"},
		{"flow generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: {by: 'human:test', at: '2026-11-17T12:00:00Z'}", 1), ""},
		{"scalar generated", strings.Replace(skillHeader, "  generated:\n    by: human:test\n    at: \"2026-11-17T12:00:00Z\"", "  generated: x", 1), "metadata.generated must be a mapping"},
		{"bare at", strings.ReplaceAll(skillHeader, "\"", ""), "metadata.generated.at must be a quoted string, for example \"2026-11-17T19:00:00Z\""},
		{"status invalid", strings.Replace(skillHeader, "---\nBody", "  status: retired\n---\nBody", 1), "metadata.status must be one of stable, draft, or deprecated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte(tc.src))
			require.NoError(t, err)
			problems := b.skillProblems()
			if tc.want == "" {
				assert.Empty(t, problems)
			} else {
				assert.Equal(t, []string{tc.want}, problems)
			}
		})
	}
	b, err := parseFrontmatter([]byte(skillHeader))
	require.NoError(t, err)
	assertSkillNodeKinds(t, b, false)
}

func assertSkillNodeKinds(t *testing.T, b *frontmatterBlock, flow bool) {
	t.Helper()
	m := b.fields["metadata"]
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
	written, err := parseFrontmatter(data)
	require.NoError(t, err)
	assert.Empty(t, written.skillProblems())
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
		{"-managed-skill", "x", "-legacy"},
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
		{"status", strings.Replace(src, "---\nBody", "  status: draft\n---\nBody", 1), true},
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

func TestSkillFixLeavesFlowAndSymlinkedSkills(t *testing.T) {
	isolateFrontmatterGit(t)
	repo := gittest.NewRepoWithCommit(t)
	t.Chdir(repo.Root)
	const flow = "---\n{name: a, description: Useful}\n---\nBody\n"
	repo.WriteFile("skills/a/SKILL.md", flow)
	repo.Run("add", ".")
	var out, errs bytes.Buffer
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-managed-skill", "skills/**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "skills/a/SKILL.md: frontmatter must be a block mapping to fix a skill\n", errs.String())
	require.NoError(t, os.MkdirAll("skills/link", 0o755))
	if err := os.Symlink("../a/SKILL.md", "skills/link/SKILL.md"); err != nil {
		t.Skip("symlinks unavailable")
	}
	repo.Run("add", "skills/link/SKILL.md")
	_, err = runFrontmatter(t.Context(), []string{"-managed-skill", "skills/link/SKILL.md", "-fix", "-by", "human:test"}, &out, &errs)
	require.ErrorContains(t, err, "skills/link/SKILL.md")
	data, err := os.ReadFile("skills/a/SKILL.md")
	require.NoError(t, err)
	assert.Equal(t, flow, string(data))
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
	// Both listed, independent skills are fixed; bundle resources remain untouched.
	n, err := runFrontmatter(t.Context(), []string{"-fix", "-by", "human:test", "-type-rule", "**=Guide", "-managed-skill", "**/SKILL.md"}, &out, &errs)
	require.NoError(t, err)
	assert.Zero(t, n, errs.String())
	for _, name := range []string{"SKILL.md", "skills/managed/SKILL.md"} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		b, err := parseFrontmatter(data)
		require.NoError(t, err)
		assert.Empty(t, b.skillProblems(), name)
	}
	for name, want := range map[string]string{"references/example.md": resource, "skills/managed/vendor/SKILL.md": missing} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		assert.Equal(t, want, string(data), name)
	}
}

func TestSkillFixFillsEmptyGeneratedMapping(t *testing.T) {
	b, err := parseFrontmatter([]byte("---\nmetadata:\n  type: Skill\n  generated: {}\n---\nBody\n"))
	require.NoError(t, err)
	changed, _ := b.fixSkill("codex")
	require.True(t, changed)
	assert.Empty(t, b.skillProblems())
	assertSkillNodeKinds(t, b, true)
}

func TestSkillFixRefusesIncompleteProfile(t *testing.T) {
	for _, tc := range []struct{ name, header, by, note string }{
		{"null metadata", "name: x\nmetadata:\n", "human:test", ""},
		{"null generated", "metadata:\n  type: Skill\n  generated:\n", "human:test", ""},
		{"by only", "metadata:\n  type: Skill\n  generated:\n    by: human:old\n", "human:new", "supply both"},
		{"at only", "metadata:\n  type: Skill\n  generated:\n    at: \"2020-01-01T00:00:00Z\"\n", "human:new", "supply both"},
		{"wrong type", "metadata:\n  type: wrong\n", "human:test", ""},
		{"invalid status", "metadata:\n  type: Skill\n  status: retired\n", "human:test", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := parseFrontmatter([]byte("---\n" + tc.header + "---\nBody\n"))
			require.NoError(t, err)
			changed, notes := b.fixSkill(tc.by)
			assert.False(t, changed)
			if tc.note != "" {
				assert.Contains(t, fmt.Sprint(notes), tc.note)
			}
		})
	}
}
