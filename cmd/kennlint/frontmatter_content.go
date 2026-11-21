package main

import (
	"bytes"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
)

func frontmatterMeaningful(a, b *frontmatterBlock, skill bool) bool {
	var x, y map[string]any
	_ = a.doc.Decode(&x)
	_ = b.doc.Decode(&y)
	for _, header := range []map[string]any{x, y} {
		if skill {
			header, _ = header["metadata"].(map[string]any)
		} else if status, ok := header["status"].(string); ok && status == "stable" {
			delete(header, "status") // Stable is the default concept status.
		}
		canonicalMetadata(header)
	}
	return !reflect.DeepEqual(x, y) || canonicalHTML(a.body) != canonicalHTML(b.body)
}

func stripProvenance(header map[string]any) {
	if generated, ok := header["generated"].(map[string]any); ok {
		delete(generated, "by")
		delete(generated, "at")
		if len(generated) == 0 {
			delete(header, "generated")
		}
	}
}

// canonicalHTML renders Markdown so that edits which render the same compare
// equal. Whitespace runs in prose text, including soft line breaks, become one
// space; code, raw HTML, attributes and hard breaks render untouched. Text
// between inline HTML tags is preserved, including nested Markdown.
func canonicalHTML(src []byte) string {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	root := parser.New(parser.WithExtensions(extension.NewGFMParser(), extension.NewFootnoteParser())).Parse(src)
	var tags []string
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if raw, ok := n.(*ast.RawHTML); ok {
			tags = updateInlineTags(tags, raw.Value.Value(src))
		}
		if t, ok := n.(*ast.Text); ok && len(tags) == 0 {
			value := t.Value.Value(src)
			if t.SoftLineBreak() {
				value += " "
			}
			t.Value = text.NewSingleLineValueFromString(proseSpace.ReplaceAllString(value, " "), text.IdentityDecoder)
			t.SetSoftLineBreak(false)
		}
		return ast.WalkContinue, nil
	})
	var rendered bytes.Buffer
	_ = html.New(html.WithUnsafe(), html.WithExtensions(extension.NewGFMHTMLRenderer(), extension.NewFootnoteHTMLRenderer())).Render(&rendered, src, root)
	return rendered.String()
}

var proseSpace = regexp.MustCompile(`[ \t\n\f]+`)

// Goldmark has already recognized the tag. Only its name and direction
// matter here; its attributes and the original markup remain untouched.
var inlineTag = regexp.MustCompile(`^<(/?)([A-Za-z][A-Za-z0-9-]*)`)

func updateInlineTags(tags []string, raw string) []string {
	m := inlineTag.FindStringSubmatch(raw)
	if m == nil {
		return tags // Comment, declaration or processing instruction.
	}
	name := strings.ToLower(m[2])
	if m[1] == "/" {
		for i := range slices.Backward(tags) {
			if tags[i] == name {
				return tags[:i]
			}
		}
		return tags
	}
	if strings.HasSuffix(raw, "/>") {
		return tags
	}
	switch name {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return tags // HTML void elements have no closing tag.
	default:
		return append(tags, name)
	}
}

// OKF v0.3 §11.2: an empty or null value is an absent key, and a list field
// accepts one item as a scalar. List members are never dropped, and the
// values of x- extension keys are kept as written.
var (
	listFields = []string{"tags", "sources", "parameters"}
	// absentMappings are defined mapping fields whose empty form is also absent.
	absentMappings = map[string]bool{"usage_window": true, "executor": true, "attester": true}
)

func canonicalMetadata(header map[string]any) {
	stripProvenance(header)
	// Verification records a review, not a change to the generated content.
	delete(header, "verified")
	pruneAbsent(header)
	for _, key := range listFields {
		if value, ok := header[key]; ok {
			if _, isList := value.([]any); !isList {
				header[key] = []any{value}
			}
		}
	}
	canonicalTimestamps(header)
}

func pruneAbsent(m map[string]any) {
	for key, value := range m {
		if !strings.HasPrefix(key, "x-") {
			pruneNested(value)
		}
		if emptyOptionalValue(value) || (absentMappings[key] && emptyMapping(value)) {
			delete(m, key)
		}
	}
}

func emptyMapping(value any) bool {
	m, ok := value.(map[string]any)
	return ok && len(m) == 0
}

func pruneNested(value any) {
	switch v := value.(type) {
	case map[string]any:
		pruneAbsent(v)
	case []any:
		for _, item := range v {
			pruneNested(item)
		}
	}
}

// canonicalTimestamps rewrites the OKF timestamp keys as UTC instants, so quoted
// and YAML-native forms and equivalent offsets compare equal.
func canonicalTimestamps(header map[string]any) {
	canonicalInstants(header, "stale_after")
	canonicalWindow(header)
	entries, _ := header["sources"].([]any)
	for _, entry := range entries {
		if source, ok := entry.(map[string]any); ok {
			canonicalInstants(source, "last_modified")
			canonicalWindow(source)
		}
	}
}

func canonicalWindow(owner map[string]any) {
	if window, ok := owner["usage_window"].(map[string]any); ok {
		canonicalInstants(window, "from", "to")
	}
}

func canonicalInstants(m map[string]any, keys ...string) {
	for _, key := range keys {
		switch v := m[key].(type) {
		case time.Time:
			m[key] = v.UTC().Format(time.RFC3339Nano)
		case string:
			if parsed, ok := parseTimestampString(v); ok {
				m[key] = parsed.UTC().Format(time.RFC3339Nano)
			}
		}
	}
}

func emptyOptionalValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	default:
		return false
	}
}
