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

// frontmatterMeaningful reports whether metadata other than provenance, or
// the rendered body, differs.
func frontmatterMeaningful(a, b *okf.Document) (bool, error) {
	x, err := metadataWithoutProvenance(a)
	if err != nil {
		return false, err
	}
	y, err := metadataWithoutProvenance(b)
	if err != nil {
		return false, err
	}
	return !reflect.DeepEqual(x, y) || (!bytes.Equal(a.Body, b.Body) && canonicalHTML(a.Body) != canonicalHTML(b.Body)), nil
}

func metadataWithoutProvenance(d *okf.Document) (map[string]any, error) {
	fields := map[string]any{}
	if err := d.Metadata.Decode(&fields); err != nil {
		return nil, err
	}
	delete(fields, "verified")
	if generated, ok := fields["generated"].(map[string]any); ok {
		delete(generated, "by")
		delete(generated, "at")
		if len(generated) == 0 {
			delete(fields, "generated")
		}
	}
	return fields, nil
}

// canonicalHTML renders Markdown so that edits which render the same compare
// equal. Whitespace runs in prose, including soft line breaks, become one
// space; code, raw HTML and hard breaks render untouched.
func canonicalHTML(src []byte) string {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	root := parser.New(parser.WithExtensions(extension.NewGFMParser(), extension.NewFootnoteParser())).Parse(src)
	_ = ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
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
