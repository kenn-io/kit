package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"gopkg.in/yaml.v3"
)

type (
	frontmatterRule  struct{ glob, kind string }
	frontmatterRules []frontmatterRule
)

func (r *frontmatterRules) String() string {
	var s []string
	for _, v := range *r {
		s = append(s, v.glob+"="+v.kind)
	}
	return strings.Join(s, ", ")
}

func (r *frontmatterRules) Set(s string) error {
	glob, kind, ok := strings.Cut(s, "=")
	glob, kind = strings.TrimSpace(glob), strings.TrimSpace(kind)
	if !ok || glob == "" || kind == "" {
		return fmt.Errorf("invalid type rule %q: expected GLOB=TYPE", s)
	}
	var e frontmatterExcludes
	if err := e.Set(glob); err != nil {
		return err
	}
	*r = append(*r, frontmatterRule{glob, kind})
	return nil
}

var frontmatterNow = time.Now

func validString(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && strings.TrimSpace(n.Value) != ""
}

func validTimestamp(n *yaml.Node) bool {
	_, ok := parseTimestamp(n)
	return ok
}

func parseTimestamp(n *yaml.Node) (time.Time, bool) {
	if n == nil || n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!timestamp") || !strings.HasSuffix(n.Value, "Z") {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, n.Value)
	return parsed, err == nil
}

func mappingField(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func appendField(n *yaml.Node, key string, value *yaml.Node) {
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func stringNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func (b *frontmatterBlock) okfProblems() []string {
	var p []string
	for _, k := range []string{"type", "title", "description"} {
		if !validString(b.fields[k]) {
			p = append(p, k+" must be a non-empty string")
		}
	}
	if status := b.fields["status"]; status != nil &&
		(!validString(status) || (status.Value != "stable" && status.Value != "draft" && status.Value != "deprecated")) {
		p = append(p, "status must be one of stable, draft, or deprecated")
	}
	g := b.fields["generated"]
	if g != nil && g.Kind != yaml.MappingNode {
		return append(p, "generated must be a mapping")
	}
	if !validString(mappingField(g, "by")) {
		p = append(p, "generated.by must be a non-empty string")
	}
	if !validTimestamp(mappingField(g, "at")) {
		p = append(p, "generated.at must be an RFC 3339 UTC timestamp ending in Z")
	}
	return p
}

func headingTitle(body []byte) string {
	n := parser.New().Parse(body).FirstChild()
	h, ok := n.(*ast.Heading)
	if !ok || h.Level != 1 {
		return ""
	}
	var parts []string
	_ = ast.Walk(h, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			switch v := n.(type) {
			case *ast.Text:
				parts = append(parts, v.Value.Value(body))
				if v.SoftLineBreak() || v.HardLineBreak() {
					parts = append(parts, " ")
				}
			case *ast.AutoLink:
				parts = append(parts, v.Label.Value(body))
			case *ast.CodeSpan:
				parts = append(parts, v.Value.Value(body))
			}
		}
		return ast.WalkContinue, nil
	})
	return strings.TrimSpace(strings.Join(parts, ""))
}

func (b *frontmatterBlock) fixOKF(file string, rules frontmatterRules, by, description string) (bool, []string) {
	var notes []string
	for _, key := range []string{"type", "title", "description"} {
		if n := b.fields[key]; n != nil && !validString(n) {
			return false, nil
		}
	}
	if g := b.fields["generated"]; g != nil {
		if g.Kind != yaml.MappingNode {
			return false, nil
		}
		if n := mappingField(g, "by"); n != nil && !validString(n) {
			return false, nil
		}
		if n := mappingField(g, "at"); n != nil && !validTimestamp(n) {
			return false, nil
		}
	}
	changed := false
	root := b.doc.Content[0]
	put := func(k, v string) {
		if b.fields[k] == nil && strings.TrimSpace(v) != "" {
			n := stringNode(v)
			appendField(root, k, n)
			b.fields[k] = n
			changed = true
		}
	}
	if b.fields["type"] == nil {
		var matches []string
		kind := ""
		ambiguous := false
		for _, r := range rules {
			if frontmatterGlob(r.glob, file) {
				matches = append(matches, r.glob+"="+r.kind)
				if kind != "" && kind != r.kind {
					ambiguous = true
				}
				kind = r.kind
			}
		}
		if ambiguous {
			notes = append(notes, "type rules disagree: "+strings.Join(matches, ", "))
		} else if kind == "" {
			notes = append(notes, "add type: or a matching -type-rule")
		} else {
			put("type", kind)
		}
	}
	if b.fields["title"] == nil {
		put("title", headingTitle(b.body))
		if b.fields["title"] == nil {
			notes = append(notes, "add title: (first block is not an H1)")
		}
	}
	put("description", description)
	if b.fields["description"] == nil {
		notes = append(notes, "supply -description TEXT")
	}
	g := b.fields["generated"]
	if g == nil && strings.TrimSpace(by) != "" {
		g = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendField(root, "generated", g)
		b.fields["generated"] = g
		changed = true
	}
	if g == nil {
		notes = append(notes, "supply -by ACTOR")
	} else if g.Kind == yaml.MappingNode {
		if mappingField(g, "by") == nil && strings.TrimSpace(by) != "" {
			appendField(g, "by", stringNode(by))
			changed = true
		}
		if mappingField(g, "by") == nil {
			notes = append(notes, "supply -by ACTOR")
		}
		if validString(mappingField(g, "by")) && mappingField(g, "at") == nil {
			appendField(g, "at", stringNode(frontmatterNow().UTC().Truncate(time.Second).Format(time.RFC3339)))
			changed = true
		}
	}
	if validString(mappingField(g, "by")) && validTimestamp(mappingField(g, "at")) {
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "last_edited" {
				root.Content = append(root.Content[:i], root.Content[i+2:]...)
				delete(b.fields, "last_edited")
				changed = true
				break
			}
		}
	}
	return changed, notes
}
