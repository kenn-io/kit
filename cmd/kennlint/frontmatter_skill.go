package main

import "gopkg.in/yaml.v3"

func (b *frontmatterBlock) skillProblems() []string {
	m := b.fields["metadata"]
	if m == nil || m.Kind != yaml.MappingNode || m.Style&yaml.FlowStyle != 0 {
		return []string{"metadata must be a block mapping"}
	}
	var p []string
	if n := mappingField(m, "type"); !validString(n) || n.Value != "Skill" {
		p = append(p, "metadata.type must be the string Skill")
	}
	p = append(p, generatedProblems(mappingField(m, "generated"), "metadata.generated", true)...)
	if n := mappingField(m, "status"); n != nil && !validStatus(n) {
		p = append(p, "metadata.status must be one of stable, draft, or deprecated")
	}
	return p
}

// fixSkill fills missing values in memory and reports a change only when the
// result is complete; otherwise nothing is written, so a present invalid
// value is never rewritten.
func (b *frontmatterBlock) fixSkill(by string) (bool, map[string]string) {
	root := b.doc.Content[0]
	if len(b.skillProblems()) == 0 {
		return false, nil
	}
	if root.Style&yaml.FlowStyle != 0 {
		// Block nodes appended to a flow root would encode as flow.
		return false, map[string]string{"metadata": "frontmatter must be a block mapping to fix a skill"}
	}
	m := b.fields["metadata"]
	if m == nil {
		m = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendField(root, "metadata", m)
		b.fields["metadata"] = m
	}
	if mappingField(m, "type") == nil {
		appendField(m, "type", stringNode("Skill"))
	}
	_, notes := fillGenerated(m, by, "metadata.generated", true)
	return len(b.skillProblems()) == 0, notes
}
