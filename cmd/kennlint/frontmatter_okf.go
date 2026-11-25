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
	if err := (&frontmatterExcludes{}).Set(glob); err != nil {
		return err
	}
	*r = append(*r, frontmatterRule{glob, kind})
	return nil
}

// typeForFile returns the type every matching rule agrees on, or "".
func typeForFile(rules frontmatterRules, file string) string {
	kind := ""
	for _, rule := range rules {
		if frontmatterGlob(rule.glob, file) {
			if kind != "" && kind != rule.kind {
				return ""
			}
			kind = rule.kind
		}
	}
	return kind
}

func nonEmptyString(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && strings.TrimSpace(n.Value) != ""
}

// profileProblems checks the Kenn profile: OKF's required type plus title,
// description, and generated provenance. Each message says how to fix it.
func profileProblems(d *okf.Document) []string {
	var p []string
	required := []struct{ key, hint string }{
		{"type", "set it, or when absent use -fix with one matching -type-rule"},
		{"title", "set it, or when absent use -fix with an H1 as the first block"},
		{"description", "set it, or when absent use -fix -description TEXT for one file"},
	}
	for _, f := range required {
		if !nonEmptyString(d.Field(f.key)) {
			p = append(p, fmt.Sprintf("%s must be a non-empty string; %s", f.key, f.hint))
		}
	}
	if s := d.Field("status"); s != nil && s.Tag != "!!null" && s.Tag != "!!str" {
		p = append(p, "status must be a string")
	}
	if g := d.Field("generated"); g != nil && g.Kind != yaml.MappingNode {
		return append(p, "generated must be a mapping")
	}
	if !nonEmptyString(d.Field("generated", "by")) {
		p = append(p, "generated.by must be a non-empty string; use -fix -by ACTOR when generated is absent")
	}
	if _, ok := okf.Timestamp(d.Field("generated", "at")); !ok {
		p = append(p, "generated.at must be an RFC 3339 timestamp with an offset or a YYYY-MM-DD date")
	}
	return p
}

func headingTitle(body []byte) string {
	h, ok := parser.New().Parse(body).FirstChild().(*ast.Heading)
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

// fixOKF fills absent fields whose values are known and never touches
// present ones. generated is written only when it is absent entirely.
func fixOKF(d *okf.Document, file string, rules frontmatterRules, by, description string) bool {
	changed := false
	put := func(n *yaml.Node, key, value string) {
		if value != "" && d.Field(key) == nil {
			n.Content = append(n.Content, stringNode(key), stringNode(value))
			changed = true
		}
	}
	put(&d.Metadata, "type", typeForFile(rules, file))
	if d.Field("title") == nil {
		put(&d.Metadata, "title", headingTitle(d.Body))
	}
	put(&d.Metadata, "description", description)
	if by != "" && d.Field("generated") == nil {
		g := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle}
		g.Content = []*yaml.Node{
			stringNode("by"), stringNode(by),
			stringNode("at"), stringNode(time.Now().UTC().Format(time.RFC3339)),
		}
		d.Metadata.Content = append(d.Metadata.Content, stringNode("generated"), g)
		changed = true
	}
	return changed
}

func stringNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}
