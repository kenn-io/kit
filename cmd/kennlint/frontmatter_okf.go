package main

import (
	"fmt"
	"slices"
	"strconv"
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

func validString(n *yaml.Node) bool {
	n = resolveYAML(n)
	return n != nil && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && strings.TrimSpace(n.Value) != ""
}

func parseTimestamp(n *yaml.Node) (time.Time, bool) {
	n = resolveYAML(n)
	if n == nil || n.Kind != yaml.ScalarNode || (n.Tag != "!!str" && n.Tag != "!!timestamp") {
		return time.Time{}, false
	}
	return parseTimestampString(n.Value)
}

func parseTimestampString(value string) (time.Time, bool) {
	layout := time.RFC3339
	if len(value) == len(time.DateOnly) {
		layout = time.DateOnly
	}
	value = strings.ToUpper(value)
	parsed, err := time.Parse(layout, value)
	return parsed, err == nil && validRFC3339Offset(value)
}

// validRFC3339Offset rejects numeric offsets that RFC 3339 forbids but Go's
// parser accepts: hours above 23 and minutes above 59.
func validRFC3339Offset(value string) bool {
	if len(value) < len("+00:00") {
		return true
	}
	offset := value[len(value)-len("+00:00"):]
	if offset[0] != '+' && offset[0] != '-' {
		return true
	}
	hours, hoursErr := strconv.Atoi(offset[1:3])
	minutes, minutesErr := strconv.Atoi(offset[4:])
	return hoursErr == nil && minutesErr == nil && hours <= 23 && minutes <= 59
}

func mappingField(n *yaml.Node, key string) *yaml.Node {
	n = resolveYAML(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return resolveYAML(n.Content[i+1])
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
	p := sourcesProblems(b.fields["sources"], "sources")
	if b.fields["last_edited"] != nil {
		p = append(p, "last_edited is replaced by generated.at; remove it")
	}
	for _, k := range []string{"type", "title", "description"} {
		if !validString(b.fields[k]) {
			p = append(p, k+" must be a non-empty string")
		}
	}
	if status := b.fields["status"]; !emptyOptional(status) &&
		!validString(status) {
		p = append(p, "status must be a non-empty string")
	}
	g := b.fields["generated"]
	if g != nil && g.Kind != yaml.MappingNode {
		return append(p, "generated must be a mapping")
	}
	if !validString(mappingField(g, "by")) {
		p = append(p, "generated.by must be a non-empty string")
	}
	if _, ok := parseTimestamp(mappingField(g, "at")); !ok {
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
func (b *frontmatterBlock) fixOKF(file string, rules frontmatterRules, by, description string) (bool, map[string]string) {
	notes := map[string]string{}
	for _, key := range []string{"type", "title", "description"} {
		if n := b.fields[key]; n != nil && !validString(n) {
			return false, nil
		}
	}
	g := b.fields["generated"]
	oldBy, oldAt := mappingField(g, "by"), mappingField(g, "at")
	_, validAt := parseTimestamp(oldAt)
	if (g != nil && g.Kind != yaml.MappingNode) || (oldBy != nil && !validString(oldBy)) || (oldAt != nil && !validAt) {
		return false, nil
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
			notes["type"] = "type rules disagree: " + strings.Join(matches, ", ")
		} else if kind == "" {
			notes["type"] = "add type: or a matching -type-rule"
		} else {
			put("type", kind)
		}
	}
	if b.fields["title"] == nil {
		put("title", headingTitle(b.body))
		if b.fields["title"] == nil {
			notes["title"] = "add title: (first block is not an H1)"
		}
	}
	put("description", description)
	if b.fields["description"] == nil {
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
		} else if validString(b.fields["type"]) && validString(b.fields["title"]) && validString(b.fields["description"]) {
			if g == nil {
				g = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				appendField(root, "generated", g)
				b.fields["generated"] = g
			}
			g.Style |= yaml.FlowStyle
			appendField(g, "by", stringNode(by))
			appendField(g, "at", stringNode(frontmatterNow().UTC().Truncate(time.Second).Format(time.RFC3339)))
			if i := slices.Index(root.Content, b.fields["last_edited"]); i > 0 {
				root.Content = slices.Delete(root.Content, i-1, i+1)
				delete(b.fields, "last_edited")
			}
			changed = true
		}
	}
	return changed, notes
}

// resolveYAML reads aliases as their values. parseFrontmatter has already decoded
// the document, so recursive or dangling aliases have been rejected by yaml.v3.
func resolveYAML(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
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
	n = resolveYAML(n)
	if n == nil {
		return nil
	}
	copyNode := *n
	copyNode.Anchor = ""
	copyNode.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		copyNode.Content[i] = cloneYAMLValue(child)
	}
	return &copyNode
}
