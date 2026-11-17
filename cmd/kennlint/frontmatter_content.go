package main

import (
	"bytes"
	"reflect"
	"strings"

	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"
)

func frontmatterMeaningful(a, b *frontmatterBlock) bool {
	var x, y map[string]any
	_ = a.doc.Decode(&x)
	_ = b.doc.Decode(&y)
	stripProvenance(x)
	stripProvenance(y)
	return !reflect.DeepEqual(x, y) || canonicalHTML(a.body) != canonicalHTML(b.body)
}

func stripProvenance(header map[string]any) {
	delete(header, "last_edited")
	if generated, ok := header["generated"].(map[string]any); ok {
		delete(generated, "by")
		delete(generated, "at")
		if len(generated) == 0 {
			delete(header, "generated")
		}
	}
}

func canonicalHTML(src []byte) string {
	src = bytes.ReplaceAll(src, []byte("\r\n"), []byte("\n"))
	root := parser.New(parser.WithExtensions(extension.NewGFMParser())).Parse(src)
	var rendered bytes.Buffer
	_ = html.New(html.WithUnsafe(), html.WithExtensions(extension.NewGFMHTMLRenderer())).Render(&rendered, src, root)
	s := rendered.String()
	var out strings.Builder
	pre, space := false, false
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "<pre>") {
			pre = true
		}
		if strings.HasPrefix(s[i:], "</pre>") {
			pre = false
		}
		c := s[i]
		if !pre && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f') {
			if !space {
				out.WriteByte(' ')
				space = true
			}
		} else {
			out.WriteByte(c)
			space = false
		}
		i++
	}
	return out.String()
}
