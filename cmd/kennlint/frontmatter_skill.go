package main

import (
	"strings"
	"time"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/okf"
	"gopkg.in/yaml.v3"
)

var frontmatterNow = time.Now

func skillProblems(b *okf.Document) []string {
	m := b.Field("metadata")
	if m == nil || m.Kind != yaml.MappingNode {
		return []string{"metadata must be a mapping"}
	}
	var p []string
	if n := mappingField(m, "type"); n == nil || !nonEmptyString(n) || n.Value != "Skill" {
		p = append(p, "metadata.type must be the string Skill")
	}
	p = append(p, generatedProblems(mappingField(m, "generated"), "metadata.generated")...)
	if n := mappingField(m, "status"); !emptyOptional(n) && !nonEmptyString(n) {
		p = append(p, "metadata.status must be a non-empty string")
	}
	return p
}

// fixSkill fills missing values in memory and reports a change only when the
// result is complete; otherwise nothing is written, so a present invalid
// value is never rewritten.
func fixSkill(b *okf.Document, by string) (bool, map[string]string) {
	root := &b.Metadata
	if len(skillProblems(b)) == 0 {
		return false, nil
	}

	materializeYAMLAliases(root)
	m := b.Field("metadata")
	if m != nil && m.Kind != yaml.MappingNode {
		return false, nil
	}
	if m == nil {
		m = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendField(root, "metadata", m)
	}
	if mappingField(m, "type") == nil {
		appendField(m, "type", stringNode("Skill"))
	}
	notes := fillGenerated(m, by, "metadata.generated")
	return len(skillProblems(b)) == 0, notes
}

func generatedProblems(g *yaml.Node, path string) []string {
	if g != nil && g.Kind != yaml.MappingNode {
		return []string{path + " must be a mapping"}
	}
	var p []string
	if !nonEmptyString(mappingField(g, "by")) {
		p = append(p, path+".by must be a non-empty string")
	}
	at := mappingField(g, "at")
	_, validAt := okf.Timestamp(at)
	if !validAt {
		p = append(p, path+".at must be an RFC 3339 timestamp with an offset or a YYYY-MM-DD date")
	}
	return p
}

func fillGenerated(parent *yaml.Node, by, path string) map[string]string {
	g := mappingField(parent, "generated")
	if g != nil && g.Kind != yaml.MappingNode {
		return nil
	}
	oldBy, oldAt := mappingField(g, "by"), mappingField(g, "at")
	if (oldBy == nil) != (oldAt == nil) {
		missing := "at"
		if oldBy == nil {
			missing = "by"
		}
		return map[string]string{path + "." + missing: path + "." + missing + " is missing; supply both " + path + ".by and " + path + ".at by hand"}
	}
	if oldBy != nil {
		return nil
	}
	if by == "" {
		return map[string]string{path + ".by": path + ".by: supply -by ACTOR"}
	}
	if g == nil {
		g = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendField(parent, "generated", g)
	}
	g.Style |= yaml.FlowStyle
	appendField(g, "by", stringNode(by))
	appendField(g, "at", stringNode(frontmatterNow().UTC().Truncate(time.Second).Format(time.RFC3339)))
	return nil
}

func appendField(n *yaml.Node, key string, value *yaml.Node) {
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func emptyOptional(n *yaml.Node) bool {
	return n == nil || n.Tag == "!!null" || (n.Kind == yaml.ScalarNode && n.Tag == "!!str" && n.Value == "") || (n.Kind == yaml.SequenceNode && len(n.Content) == 0)
}

// materializeYAMLAliases keeps each alias use independent before a fixer can
// change its anchor. This also preserves aliases whose definitions are removed.
func materializeYAMLAliases(n *yaml.Node) {
	for i, child := range n.Content {
		if child.Kind == yaml.AliasNode {
			n.Content[i] = cloneYAMLValue(child)
		} else {
			materializeYAMLAliases(child)
		}
	}
}

func cloneYAMLValue(n *yaml.Node) *yaml.Node {
	// This tree has already been decoded, so aliases have valid targets.
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	copyNode := *n
	copyNode.Anchor = ""
	copyNode.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		copyNode.Content[i] = cloneYAMLValue(child)
	}
	return &copyNode
}

// mappingField uses the same effective YAML lookup as ordinary documents.
func mappingField(n *yaml.Node, key string) *yaml.Node {
	if n == nil {
		return nil
	}
	return (&okf.Document{Metadata: *n}).Field(key)
}

func checkManagedSkill(full string, src []byte, fix bool, by string) ([]string, bool, error) {
	d, err := okf.Parse(src)
	if err != nil {
		return []string{err.Error()}, false, nil //nolint:nilerr // Invalid skill metadata is a finding.
	}
	var changed bool
	var notes map[string]string
	if fix {
		changed, notes = fixSkill(d, by)
	}
	problems := skillProblems(d)
	for i, p := range problems {
		if msg, ok := notes[strings.Fields(p)[0]]; ok {
			problems[i] = msg
		}
	}
	if !changed {
		return problems, false, nil
	}
	data, err := d.Marshal()
	if err != nil {
		return nil, false, err
	}
	if err := atomicfile.WriteFile(full, data, atomicfile.WithPreserveMode(), atomicfile.WithFollowLink()); err != nil {
		return nil, false, err
	}
	return problems, true, nil
}
