package main

import (
	"bytes"
	"reflect"
	"regexp"

	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
	"github.com/yuin/goldmark/v2/text"
)

func frontmatterMeaningfulProfile(a, b *frontmatterBlock, skill bool) bool {
	var x, y map[string]any
	_ = a.doc.Decode(&x)
	_ = b.doc.Decode(&y)
	if skill {
		if m, ok := x["metadata"].(map[string]any); ok {
			stripProvenance(m)
		}
		if m, ok := y["metadata"].(map[string]any); ok {
			stripProvenance(m)
		}
	} else {
		stripProvenance(x)
		stripProvenance(y)
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
// in a block containing inline HTML is preserved conservatively.
func canonicalHTML(src []byte) string {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	root := parser.New(parser.WithExtensions(extension.NewGFMParser())).Parse(src)
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
	_ = html.New(html.WithUnsafe(), html.WithExtensions(extension.NewGFMHTMLRenderer())).Render(&rendered, src, root)
	return rendered.String()
}

var proseSpace = regexp.MustCompile(`[ \t\n\f]+`)
