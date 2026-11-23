package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/parser"
	"gopkg.in/yaml.v3"

	"go.kenn.io/kit/okf"
)

type (
	frontmatterRule  struct{ glob, kind string }
	frontmatterRules []frontmatterRule
)

func (r *frontmatterRules) String() string { return fmt.Sprint(*r) }

func (r *frontmatterRules) Set(s string) error {
	glob, kind, ok := strings.Cut(s, "=")
	glob, kind = strings.TrimSpace(glob), strings.TrimSpace(kind)
	if !ok || glob == "" || kind == "" {
		return fmt.Errorf("invalid type rule %q: expected GLOB=TYPE", s)
	}
	e := frontmatterExcludes{}
	if err := e.Set(glob); err != nil {
		return err
	}
	*r = append(*r, frontmatterRule{glob, kind})
	return nil
}

func typeForFile(rules frontmatterRules, file string) (kind, problem string) {
	var first frontmatterRule
	for _, rule := range rules {
		if !frontmatterGlob(rule.glob, file) {
			continue
		}
		if first.kind != "" && first.kind != rule.kind {
			return "", fmt.Sprintf("type rules disagree: %s=%s, %s=%s", first.glob, first.kind, rule.glob, rule.kind)
		}
		first = rule
	}
	if first.kind == "" {
		return "", "add type: or a matching -type-rule"
	}
	return first.kind, ""
}

func appendField(n *yaml.Node, key string, value *yaml.Node) {
	n.Content = append(n.Content, stringNode(key), value)
}

func stringNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func profileProblems(b *okf.Document) []string {
	p := b.ConceptProblems()
	for _, k := range []string{"title", "description"} {
		if !okf.NonEmptyString(b.Field(k)) {
			p = append(p, k+" must be a non-empty string")
		}
	}
	g := b.Field("generated")
	if g != nil && g.Kind != yaml.MappingNode {
		return append(p, "generated must be a mapping")
	}
	if !okf.NonEmptyString(b.Field("generated", "by")) {
		p = append(p, "generated.by must be a non-empty string")
	}
	if _, ok := okf.Timestamp(b.Field("generated", "at")); !ok {
		p = append(p, "generated.at must be an RFC 3339 timestamp with an offset or a YYYY-MM-DD date")
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
			case *ast.Image:
				return ast.WalkSkipChildren, nil
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

// fixOKF fills missing values and returns, by field, what the author must supply.
func fixOKF(b *okf.Document, file string, rules frontmatterRules, by, description string) (bool, map[string]string) {
	notes := map[string]string{}
	for _, key := range []string{"type", "title", "description"} {
		if n := b.Field(key); n != nil && !okf.NonEmptyString(n) {
			return false, nil
		}
	}
	g := b.Field("generated")
	oldBy, oldAt := b.Field("generated", "by"), b.Field("generated", "at")
	_, validAt := okf.Timestamp(oldAt)
	if (g != nil && g.Kind != yaml.MappingNode) || (oldBy != nil && !okf.NonEmptyString(oldBy)) || (oldAt != nil && !validAt) {
		return false, nil
	}
	changed := false
	root := &b.Metadata
	put := func(key, value, missing string) {
		if value == "" {
			notes[key] = missing
			return
		}
		appendField(root, key, stringNode(value))
		changed = true
	}
	if b.Field("type") == nil {
		kind, problem := typeForFile(rules, file)
		put("type", kind, problem)
	}
	if b.Field("title") == nil {
		put("title", headingTitle(b.Body), "add title: (first block is not an H1)")
	}
	if b.Field("description") == nil {
		put("description", description, "supply -description TEXT")
	}
	if (oldBy == nil) != (oldAt == nil) {
		missing := "generated.by"
		if oldAt == nil {
			missing = "generated.at"
		}
		notes[missing] = missing + " is missing; supply both generated.by and generated.at by hand"
		return changed, notes
	}
	if oldBy != nil {
		return changed, notes
	}
	if by == "" {
		notes["generated.by"] = "generated.by: supply -by ACTOR"
	}
	if len(notes) != 0 {
		return changed, notes
	}
	if g == nil {
		g = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		appendField(root, "generated", g)
	}
	g.Style |= yaml.FlowStyle
	appendField(g, "by", stringNode(by))
	appendField(g, "at", stringNode(time.Now().UTC().Format(time.RFC3339)))
	return true, notes
}
