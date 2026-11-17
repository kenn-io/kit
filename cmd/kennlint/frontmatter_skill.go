package main

import "gopkg.in/yaml.v3"

func (b *frontmatterBlock) skillProblems() []string {
	problems, _ := b.skillValidation()
	return problems
}

// skillValidation distinguishes missing fields the fixer can fill from present invalid values.
func (b *frontmatterBlock) skillValidation() ([]string, bool) {
	m := b.fields["metadata"]
	if m == nil {
		return []string{"metadata must be a block mapping"}, false
	}
	if m.Kind != yaml.MappingNode || m.Style&yaml.FlowStyle != 0 {
		return []string{"metadata must be a block mapping"}, true
	}
	var p []string
	invalid := false
	if n := mappingField(m, "type"); !validString(n) || n.Value != "Skill" {
		p = append(p, "metadata.type must be the string Skill")
		invalid = n != nil
	}
	g := mappingField(m, "generated")
	gp := generatedProblems(g, "metadata.generated", skillGeneratedProfile)
	p = append(p, gp...)
	missing := 0
	if g == nil {
		missing = 2
	} else if g.Kind == yaml.MappingNode && g.Style&yaml.FlowStyle == 0 {
		if mappingField(g, "by") == nil {
			missing++
		}
		if mappingField(g, "at") == nil {
			missing++
		}
	}
	invalid = invalid || len(gp) > missing
	if n := mappingField(m, "status"); n != nil && !validStatus(n) {
		p = append(p, "metadata.status must be one of stable, draft, or deprecated")
		invalid = true
	}
	return p, invalid
}

func (b *frontmatterBlock) fixSkill(by string) (bool, map[string]string) {
	if b.doc.Content[0].Style&yaml.FlowStyle != 0 {
		return false, map[string]string{"frontmatter": "frontmatter must be a block mapping to fix a skill"}
	}
	if _, invalid := b.skillValidation(); invalid {
		return false, map[string]string{"invalid": "correct invalid skill metadata before fixing"}
	}
	m := b.fields["metadata"]
	changed := false
	if m == nil {
		m = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendField(b.doc.Content[0], "metadata", m)
		b.fields["metadata"] = m
		changed = true
	}
	if mappingField(m, "type") == nil {
		appendField(m, "type", stringNode("Skill"))
		changed = true
	}
	filled, notes := fillGenerated(m, by, "metadata.generated", true, skillGeneratedProfile)
	if len(b.skillProblems()) != 0 {
		return false, notes
	}
	return changed || filled, notes
}
