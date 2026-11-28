package main

import (
	"bytes"
	"reflect"
	"regexp"
	"slices"
	"strings"

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
	return profileMeaningful(a, b, false)
}

func profileMeaningful(a, b *okf.Document, skill bool) (bool, error) {
	x, err := metadataWithoutProvenance(a, skill)
	if err != nil {
		return false, err
	}
	y, err := metadataWithoutProvenance(b, skill)
	if err != nil {
		return false, err
	}
	return !reflect.DeepEqual(x, y) || (!bytes.Equal(a.Body, b.Body) && canonicalHTML(a.Body) != canonicalHTML(b.Body)), nil
}

func metadataWithoutProvenance(d *okf.Document, skill bool) (map[string]any, error) {
	fields := map[string]any{}
	if err := d.Metadata.Decode(&fields); err != nil {
		return nil, err
	}
	header := fields
	if skill {
		header, _ = fields["metadata"].(map[string]any)
	}
	// Omitted status and explicit stable are equivalent under the Kenn profile.
	status := header["status"]
	emptyList, list := status.([]any)
	if status == "stable" || (skill && (status == nil || status == "" || (list && len(emptyList) == 0))) {
		delete(header, "status")
	}
	delete(header, "verified")
	if generated, ok := header["generated"].(map[string]any); ok {
		delete(generated, "by")
		delete(generated, "at")
		if len(generated) == 0 {
			delete(header, "generated")
		}
	}
	return fields, nil
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
