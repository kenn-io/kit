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
	codeDepth, preDepth := 0, 0
	space := false
	for i := 0; i < len(s); {
		if s[i] == '<' {
			end, quote := i+1, byte(0)
			for end < len(s) {
				c := s[end]
				if quote != 0 {
					if c == quote {
						quote = 0
					}
				} else if c == '\'' || c == '"' {
					quote = c
				} else if c == '>' {
					break
				}
				end++
			}
			if end < len(s) {
				tag := s[i : end+1]
				name := strings.TrimSpace(tag[1 : len(tag)-1])
				closing := strings.HasPrefix(name, "/")
				name = strings.TrimPrefix(name, "/")
				name, _, _ = strings.Cut(name, " ")
				name, _, _ = strings.Cut(name, "\n")
				name, _, _ = strings.Cut(name, "\t")
				if !strings.HasSuffix(strings.TrimSpace(tag[:len(tag)-1]), "/") {
					switch strings.ToLower(name) {
					case "code":
						if closing {
							if codeDepth > 0 {
								codeDepth--
							}
						} else {
							codeDepth++
						}
					case "pre":
						if closing {
							if preDepth > 0 {
								preDepth--
							}
						} else {
							preDepth++
						}
					}
				}
				out.WriteString(tag)
				space = false
				i = end + 1
				continue
			}
		}
		c := s[i]
		if codeDepth == 0 && preDepth == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f') {
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
