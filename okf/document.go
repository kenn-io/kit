// Package okf reads and edits the YAML metadata in Open Knowledge Format
// v0.3 Markdown files. Parse accepts partial metadata, so consumers can apply
// their own profile or read a bundle index without treating it as a concept.
package okf

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ErrNoFrontmatter means the first line is not an opening delimiter.
var ErrNoFrontmatter = errors.New("missing opening --- frontmatter delimiter")

// Document contains an editable YAML mapping and the uninterpreted Markdown body.
type Document struct {
	Metadata yaml.Node
	Body     []byte
	newline  string
}

// New creates an empty metadata mapping for body, using its first line ending.
func New(body []byte) *Document {
	newline := "\n"
	if line, _, found := bytes.Cut(body, []byte("\n")); found && bytes.HasSuffix(line, []byte("\r")) {
		newline = "\r\n"
	}
	return &Document{Metadata: yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, Body: body, newline: newline}
}

// Parse reads a single delimited mapping and retains the remaining bytes as the body.
// Metadata must use direct values. Anchors and aliases are unsupported.
// Body shares its backing array with src.
func Parse(src []byte) (*Document, error) {
	line, content, _ := bytes.Cut(src, []byte("\n"))
	if string(bytes.TrimSuffix(line, []byte("\r"))) != "---" {
		return nil, ErrNoFrontmatter
	}
	if content == nil {
		return nil, errors.New("missing closing --- frontmatter delimiter")
	}
	d := New(src)
	var header []byte
	for rest := content; rest != nil; {
		line, body, _ := bytes.Cut(rest, []byte("\n"))
		if string(bytes.TrimSuffix(line, []byte("\r"))) == "---" {
			header = content[:len(content)-len(rest)]
			d.Body = body
			break
		}
		rest = body
	}
	if header == nil {
		return nil, errors.New("missing closing --- frontmatter delimiter")
	}
	dec := yaml.NewDecoder(bytes.NewReader(header))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("invalid frontmatter YAML: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("frontmatter must contain a single YAML mapping")
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("frontmatter must be a YAML mapping")
	}
	d.Metadata = *doc.Content[0]
	if err := prepareMapping(&d.Metadata); err != nil {
		return nil, err
	}
	// Decoding into a map also checks for duplicate keys at every depth.
	var values map[string]any
	if err := d.Metadata.Decode(&values); err != nil {
		return nil, fmt.Errorf("invalid frontmatter YAML: %w", err)
	}
	return d, nil
}

func prepareMapping(n *yaml.Node) error {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return errors.New("frontmatter does not support YAML anchors and aliases")
	}
	// yaml.v3 assigns a YAML 1.1 merge tag to plain <<. In the YAML 1.2
	// core schema used by OKF it is a string.
	if n.Tag == "!!merge" && n.Value == "<<" {
		n.Tag = "!!str"
	}
	for _, child := range n.Content {
		if err := prepareMapping(child); err != nil {
			return err
		}
	}
	return nil
}

// Field looks up a path of mapping keys. It returns nil if the path is absent.
func (d *Document) Field(keys ...string) *yaml.Node {
	n := &d.Metadata
	for _, key := range keys {
		n = mappingField(n, key)
	}
	return n
}

func mappingField(n *yaml.Node, key string) *yaml.Node {
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i+1]
			}
		}
	}
	return nil
}

// Marshal writes the metadata with the original delimiter line ending and
// appends Body verbatim.
func (d *Document) Marshal() ([]byte, error) {
	var header bytes.Buffer
	enc := yaml.NewEncoder(&header)
	enc.SetIndent(2)
	if err := enc.Encode(&d.Metadata); err != nil {
		return nil, fmt.Errorf("encoding frontmatter: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	newline := d.newline
	if newline == "" {
		newline = "\n"
	}
	result := []byte("---" + newline)
	result = append(result, bytes.ReplaceAll(header.Bytes(), []byte("\n"), []byte(newline))...)
	result = append(result, "---"+newline...)
	result = append(result, d.Body...)
	if _, err := Parse(result); err != nil {
		return nil, fmt.Errorf("encoded frontmatter is invalid: %w", err)
	}
	return result, nil
}
