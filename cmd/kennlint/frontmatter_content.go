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

	"go.kenn.io/kit/okf"
)

func frontmatterMeaningful(a, b *okf.Document) (bool, error) {
	x, err := a.NormalizedMetadata()
	if err != nil {
		return false, err
	}
	y, err := b.NormalizedMetadata()
	if err != nil {
		return false, err
	}
	stripProvenance(x)
	stripProvenance(y)
	return !reflect.DeepEqual(x, y) || (!bytes.Equal(a.Body, b.Body) && canonicalHTML(a.Body) != canonicalHTML(b.Body)), nil
}

func stripProvenance(header map[string]any) {
	delete(header, "verified")
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
