package main

import (
	"bytes"
	"reflect"
	"regexp"
	"time"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
)

func frontmatterMeaningful(a, b *frontmatterBlock) bool {
	var x, y map[string]any
	_ = a.doc.Decode(&x)
	_ = b.doc.Decode(&y)
	canonicalMetadata(x)
	canonicalMetadata(y)
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
// in a block containing inline HTML is preserved conservatively.
func canonicalHTML(src []byte) string {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	root := parser.New(parser.WithExtensions(extension.NewGFMParser(), extension.NewFootnoteParser())).Parse(src)
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			// Inline HTML tags are siblings of their text, including nested Markdown.
			for child := range n.Children() {
				if _, ok := child.(*ast.RawHTML); ok {
					return ast.WalkSkipChildren, nil
				}
			}
		}
		if t, ok := n.(*ast.Text); ok && entering {
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

// optionalFields and listFields follow OKF v0.3 §11.2: an empty or null
// optional value is absent, and a list field accepts one item as a scalar.
// Unknown extension keys keep their values untouched.
var (
	optionalFields = []string{"status", "tags", "stale_after", "resource", "sources"}
	listFields     = []string{"tags", "sources", "parameters"}
)

func canonicalMetadata(header map[string]any) {
	stripProvenance(header)
	// Verification records a review, not a change to the generated content.
	delete(header, "verified")
	for _, key := range optionalFields {
		if emptyOptionalValue(header[key]) {
			delete(header, key)
		}
	}
	for _, key := range listFields {
		if value, ok := header[key]; ok {
			if _, isList := value.([]any); !isList {
				header[key] = []any{value}
			}
		}
	}
	canonicalTimestamps(header)
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
