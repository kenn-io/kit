package main

import (
	"fmt"
	"slices"
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

var frontmatterNow = time.Now

func appendField(n *yaml.Node, key string, value *yaml.Node) {
	n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

func stringNode(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func profileProblems(b *okf.Document) []string {
	p := b.ConceptProblems()
	if b.Field("last_edited") != nil {
		p = append(p, "last_edited is replaced by generated.at; remove it")
	}
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
	put := func(k, v string) {
		if b.Field(k) == nil && strings.TrimSpace(v) != "" {
			n := stringNode(v)
			appendField(root, k, n)
			changed = true
		}
	}
	if b.Field("type") == nil {
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
			notes["type"] = "type rules disagree: " + strings.Join(matches, ", ")
		} else if kind == "" {
			notes["type"] = "add type: or a matching -type-rule"
		} else {
			put("type", kind)
		}
	}
	if b.Field("title") == nil {
		put("title", headingTitle(b.Body))
		if b.Field("title") == nil {
			notes["title"] = "add title: (first block is not an H1)"
		}
	}
	put("description", description)
	if b.Field("description") == nil {
		notes["description"] = "supply -description TEXT"
	}
	if (oldBy == nil) != (oldAt == nil) {
		missing := "generated.by"
		if oldAt == nil {
			missing = "generated.at"
		}
		notes[missing] = missing + " is missing; supply both generated.by and generated.at by hand"
	} else if oldBy == nil {
		if by == "" {
			notes["generated.by"] = "generated.by: supply -by ACTOR"
		} else if okf.NonEmptyString(b.Field("type")) && okf.NonEmptyString(b.Field("title")) && okf.NonEmptyString(b.Field("description")) {
			if g == nil {
				g = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				appendField(root, "generated", g)
			}
			g.Style |= yaml.FlowStyle
			appendField(g, "by", stringNode(by))
			appendField(g, "at", stringNode(frontmatterNow().UTC().Truncate(time.Second).Format(time.RFC3339)))
			if i := slices.Index(root.Content, b.Field("last_edited")); i > 0 {
				root.Content = slices.Delete(root.Content, i-1, i+1)
			}
			changed = true
		}
	}
	return changed, notes
}
