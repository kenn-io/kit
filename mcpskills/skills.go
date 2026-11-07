// Package mcpskills serves caller-bundled Markdown guides through a fixed,
// read-only MCP tool. It never reads files or executes guide content.
package mcpskills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Entry describes a guide without its Markdown body.
type Entry struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// Guide contains authored Markdown supplied at server construction time.
type Guide struct {
	Entry
	Markdown string
}

// Config selects the exact tool name and the ordered, bundled guides it serves.
type Config struct {
	ToolName string
	Guides   []Guide
}

// Output is the structured metadata returned by both catalog and guide calls.
// A named call contains one entry; the Markdown is returned as text content.
type Output struct {
	Skills []Entry `json:"skills"`
}

// Catalog is an immutable snapshot, safe to use concurrently on multiple servers.
// Construct one with New; the zero value is not usable.
type Catalog struct {
	toolName string
	guides   []Guide
}

var (
	guideName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	toolName  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
)

// New validates and snapshots config. Names are exact, case-sensitive ASCII
// identifiers. Titles, summaries and Markdown must be nonblank valid UTF-8;
// their bytes are preserved. At least one guide is required.
func New(config Config) (*Catalog, error) {
	if !toolName.MatchString(config.ToolName) {
		return nil, errors.New("skills tool name must contain 1 to 128 ASCII letters, digits, underscores, hyphens or dots")
	}
	if len(config.Guides) == 0 {
		return nil, errors.New("skills catalog requires at least one guide")
	}
	seen := make(map[string]bool, len(config.Guides))
	for i, guide := range config.Guides {
		if !guideName.MatchString(guide.Name) {
			return nil, fmt.Errorf("guide %d name must match %s", i, guideName)
		}
		if seen[guide.Name] {
			return nil, fmt.Errorf("duplicate guide name %q", guide.Name)
		}
		seen[guide.Name] = true
		for _, field := range []struct{ name, value string }{
			{"title", guide.Title}, {"summary", guide.Summary}, {"Markdown", guide.Markdown},
		} {
			if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" {
				return nil, fmt.Errorf("guide %q %s must be nonblank valid UTF-8", guide.Name, field.name)
			}
		}
	}
	return &Catalog{toolName: config.ToolName, guides: slices.Clone(config.Guides)}, nil
}

// Entries returns an independent copy of the metadata in authored order.
func (c *Catalog) Entries() []Entry {
	entries := make([]Entry, len(c.guides))
	for i, guide := range c.guides {
		entries[i] = guide.Entry
	}
	return entries
}

// Instructions returns discovery text to append to existing server instructions
// before constructing the server. It does not modify any server options.
func (c *Catalog) Instructions() string {
	return fmt.Sprintf("Call %s {} to list this server's bundled how-to guides; call %s {\"name\":\"<guide-name>\"} to read a guide as Markdown. Guides are read-only and do not execute actions.", c.toolName, c.toolName)
}

// Tool returns an independent tool definition, including its schemas and hints.
// Use it with Handler when the application owns a central tool catalog.
func (c *Catalog) Tool() *mcp.Tool {
	closedWorld := false
	return &mcp.Tool{
		Name: c.toolName, Title: "Bundled how-to guides",
		Description: "List this server's fixed catalog of bundled how-to guides with no arguments ({}), or read one guide as authored Markdown with an exact name ({\"name\":\"<guide-name>\"}). This tool cannot read paths or execute skills.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closedWorld},
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1, "pattern": guideName.String()}},
		},
		OutputSchema: map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"skills"},
			"properties": map[string]any{"skills": map[string]any{
				"type": "array", "items": map[string]any{
					"type": "object", "additionalProperties": false, "required": []string{"name", "title", "summary"},
					"properties": map[string]any{"name": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"}},
				},
			}},
		},
	}
}

// Register adds the tool to server using its raw handler. The caller owns server
// instructions, authentication, authorization and any deferred tool discovery;
// register the tool outside optional sections if it must always be present.
func (c *Catalog) Register(server *mcp.Server) {
	server.AddTool(c.Tool(), c.Handler)
}

// Handler lists metadata when arguments are absent or {}, or returns the exact
// named Markdown with one metadata entry. All mutable results belong to the
// caller. Invalid arguments and unknown names return JSON-RPC invalid-params.
// Present names must be nonempty exact identifiers; null and unknown keys fail.
func (c *Catalog) Handler(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if request == nil || request.Params == nil {
		return nil, invalidParams("missing call parameters")
	}
	name, err := parseName(request.Params.Arguments)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	if name != "" {
		for _, guide := range c.guides {
			if guide.Name == name {
				return result(guide.Markdown, []Entry{guide.Entry}), nil
			}
		}
		return nil, invalidParams("unknown guide; call " + c.toolName + " {} to list available guides")
	}
	var text strings.Builder
	for _, guide := range c.guides {
		fmt.Fprintf(&text, "- %s: %s. %s\n", guide.Name, guide.Title, guide.Summary)
	}
	return result(text.String(), c.Entries()), nil
}

func result(text string, entries []Entry) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: Output{Skills: entries},
	}
}

func invalidParams(message string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: message}
}

func parseName(arguments json.RawMessage) (string, error) {
	if len(arguments) == 0 {
		return "", nil
	}
	if !utf8.Valid(arguments) {
		return "", errors.New("arguments must be valid UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return "", errors.New("arguments must be a JSON object")
	}
	var name string
	seen := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return "", errors.New("arguments must be a complete JSON object")
		}
		if key != "name" {
			return "", errors.New("unknown field; only optional name is accepted")
		}
		if seen {
			return "", errors.New("duplicate name field")
		}
		seen = true
		value, err := decoder.Token()
		if err != nil {
			return "", errors.New("name must be a nonempty guide identifier")
		}
		var ok bool
		name, ok = value.(string)
		if !ok || !guideName.MatchString(name) {
			return "", errors.New("name must be a nonempty guide identifier")
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return "", errors.New("arguments must be a complete JSON object")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("arguments must contain exactly one JSON object")
	}
	return name, nil
}
