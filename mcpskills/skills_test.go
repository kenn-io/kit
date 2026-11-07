package mcpskills_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/kit/mcpskills"
)

func fixture() mcpskills.Config {
	return mcpskills.Config{ToolName: "alpha.skills", Guides: []mcpskills.Guide{
		{Entry: {Name: "workflow", Title: "Workflow", Summary: "Start here"}, Markdown: "# Workflow\n\nAuthored text.\n"},
		{Entry: {Name: "search", Title: "Search", Summary: "Find things"}, Markdown: "# Search\n"},
	}}
}

func call(t *testing.T, catalog *mcpskills.Catalog, arguments string) *mcp.CallToolResult {
	t.Helper()
	result, err := catalog.Handler(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(arguments)}})
	require.NoError(t, err)
	return result
}

func metadata(t *testing.T, result *mcp.CallToolResult) mcpskills.Output {
	t.Helper()
	b, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	var output mcpskills.Output
	require.NoError(t, json.Unmarshal(b, &output))
	assert.NotContains(t, string(b), "Markdown")
	assert.NotContains(t, string(b), "Authored text")
	return output
}

func TestCatalog(t *testing.T) {
	config := fixture()
	catalog, err := mcpskills.New(config)
	require.NoError(t, err)
	expectedEntries := []mcpskills.Entry{
		{Name: "workflow", Title: "Workflow", Summary: "Start here"},
		{Name: "search", Title: "Search", Summary: "Find things"},
	}
	for _, arguments := range []string{"", "{}", " { } \n"} {
		result := call(t, catalog, arguments)
		assert.Equal(t, expectedEntries, metadata(t, result).Skills)
		assert.Equal(t, "- workflow: Workflow. Start here\n- search: Search. Find things\n", result.Content[0].(*mcp.TextContent).Text)
	}
	result := call(t, catalog, `{"na\u006de":"work\u0066low"}`)
	assert.Equal(t, config.Guides[0].Markdown, result.Content[0].(*mcp.TextContent).Text)
	assert.Equal(t, []mcpskills.Entry{config.Guides[0].Entry}, metadata(t, result).Skills)
	assert.False(t, result.IsError)
}

func TestInvalidArguments(t *testing.T) {
	catalog, err := mcpskills.New(fixture())
	require.NoError(t, err)
	for _, arguments := range []string{`null`, `[]`, `"workflow"`, `1`, `true`, `{`, `{} {}`, ` `, `{"name":"workflow",}`, `{"name":"workflow","extra":1}`, `{"name":"workflow","na\u006de":"workflow"}`, `{"name":"\ud800"}`, string([]byte{123, 34, 255, 34, 58, 49, 125}), `{"Name":"workflow"}`, `{"path":"workflow"}`, `{"name":null}`, `{"name":1}`, `{"name":true}`, `{"name":[]}`, `{"name":{}}`, `{"name":""}`, `{"name":" workflow"}`, `{"name":"workflow "}`, `{"name":"../workflow"}`, `{"name":"missing"}`, `{"name":"workflow","name":"workflow"}`} {
		t.Run(arguments, func(t *testing.T) {
			result, err := catalog.Handler(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(arguments)}})
			require.Error(t, err)
			assert.Nil(t, result)
			var rpcError *jsonrpc.Error
			require.ErrorAs(t, err, &rpcError)
			assert.EqualValues(t, jsonrpc.CodeInvalidParams, rpcError.Code)
		})
	}
	_, err = catalog.Handler(t.Context(), nil)
	require.Error(t, err)
	_, err = catalog.Handler(t.Context(), &mcp.CallToolRequest{})
	require.Error(t, err)
}

func TestInvalidConfig(t *testing.T) {
	cases := map[string]func(*mcpskills.Config){
		"empty tool":       func(c *mcpskills.Config) { c.ToolName = "" },
		"bad tool":         func(c *mcpskills.Config) { c.ToolName = "alpha/skills" },
		"long tool":        func(c *mcpskills.Config) { c.ToolName = strings.Repeat("x", 129) },
		"no guides":        func(c *mcpskills.Config) { c.Guides = nil },
		"duplicate":        func(c *mcpskills.Config) { c.Guides[1].Name = c.Guides[0].Name },
		"empty name":       func(c *mcpskills.Config) { c.Guides[0].Name = "" },
		"bad name":         func(c *mcpskills.Config) { c.Guides[0].Name = "../workflow" },
		"empty title":      func(c *mcpskills.Config) { c.Guides[0].Title = " " },
		"empty summary":    func(c *mcpskills.Config) { c.Guides[0].Summary = "\n" },
		"empty markdown":   func(c *mcpskills.Config) { c.Guides[0].Markdown = "\t" },
		"invalid title":    func(c *mcpskills.Config) { c.Guides[0].Title = "bad\xff" },
		"invalid summary":  func(c *mcpskills.Config) { c.Guides[0].Summary = "bad\xff" },
		"invalid markdown": func(c *mcpskills.Config) { c.Guides[0].Markdown = "bad\xff" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := fixture()
			mutate(&config)
			catalog, err := mcpskills.New(config)
			require.Error(t, err)
			assert.Nil(t, catalog)
			assert.NotContains(t, err.Error(), "Authored text")
		})
	}
}

func TestSnapshot(t *testing.T) {
	config := fixture()
	catalog, err := mcpskills.New(config)
	require.NoError(t, err)
	config.Guides[0].Name, config.Guides[0].Markdown = "changed", "changed"
	catalog.Entries()[0].Name = "changed"
	tool := catalog.Tool()
	tool.Name = "changed"
	tool.Annotations.ReadOnlyHint = false
	tool.InputSchema.(map[string]any)["type"] = "changed"
	result := call(t, catalog, `{}`)
	result.StructuredContent.(mcpskills.Output).Skills[0].Name = "changed"
	result.Content[0].(*mcp.TextContent).Text = "changed"
	assert.Equal(t, fixture().Guides[0].Markdown, call(t, catalog, `{"name":"workflow"}`).Content[0].(*mcp.TextContent).Text)
	assert.Equal(t, fixture().Guides[0].Entry, metadata(t, call(t, catalog, `{}`)).Skills[0])
	assert.Equal(t, "alpha.skills", catalog.Tool().Name)
	assert.True(t, catalog.Tool().Annotations.ReadOnlyHint)
	assert.Equal(t, "object", catalog.Tool().InputSchema.(map[string]any)["type"])
}

func TestUnknownNamesCannotSelectGuide(t *testing.T) {
	catalog, err := mcpskills.New(fixture())
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		name := hegel.Draw(ht, hegel.Text())
		arguments, err := json.Marshal(map[string]string{"name": name})
		require.NoError(ht, err)
		result, err := catalog.Handler(ht.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: arguments}})
		if name == "workflow" || name == "search" {
			require.NoError(ht, err)
		} else {
			require.Error(ht, err)
			assert.Nil(ht, result)
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestReturnedGraphsAreIndependent(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		config := fixture()
		config.Guides[0].Markdown = "# Original\n" + hegel.Draw(ht, hegel.Text())
		originalMarkdown := config.Guides[0].Markdown
		catalog, err := mcpskills.New(config)
		require.NoError(ht, err)
		list, err := catalog.Handler(ht.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}})
		require.NoError(ht, err)
		named, err := catalog.Handler(ht.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"name":"workflow"}`)}})
		require.NoError(ht, err)
		tool := catalog.Tool()
		originalTool := catalog.Tool()
		originalJSON, err := json.Marshal(originalTool)
		require.NoError(ht, err)
		config.Guides[0].Markdown = "mutated"
		list.Content[0].(*mcp.TextContent).Text = "mutated"
		list.StructuredContent.(mcpskills.Output).Skills[0].Title = "mutated"
		named.StructuredContent.(mcpskills.Output).Skills[0].Summary = "mutated"
		*tool.Annotations.OpenWorldHint = true
		tool.InputSchema.(map[string]any)["properties"].(map[string]any)["name"].(map[string]any)["pattern"] = "mutated"
		tool.OutputSchema.(map[string]any)["required"].([]string)[0] = "mutated"
		items := tool.OutputSchema.(map[string]any)["properties"].(map[string]any)["skills"].(map[string]any)["items"].(map[string]any)
		items["required"].([]string)[0] = "mutated"
		items["properties"].(map[string]any)["name"].(map[string]any)["type"] = "mutated"
		retainedJSON, err := json.Marshal(originalTool)
		require.NoError(ht, err)
		freshJSON, err := json.Marshal(catalog.Tool())
		require.NoError(ht, err)
		assert.Equal(ht, originalJSON, retainedJSON)
		assert.Equal(ht, originalJSON, freshJSON)
		fresh, err := catalog.Handler(ht.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"name":"workflow"}`)}})
		require.NoError(ht, err)
		assert.Equal(ht, originalMarkdown, named.Content[0].(*mcp.TextContent).Text)
		assert.Equal(ht, originalMarkdown, fresh.Content[0].(*mcp.TextContent).Text)
		assert.Equal(ht, fixture().Guides[0].Entry, fresh.StructuredContent.(mcpskills.Output).Skills[0])
	}, hegel.WithTestCases(1000), hegel.WithDatabase(""))
}

func TestUnknownNameErrorIsBounded(t *testing.T) {
	catalog, err := mcpskills.New(fixture())
	require.NoError(t, err)
	arguments, err := json.Marshal(map[string]string{"name": strings.Repeat("a", 1<<20)})
	require.NoError(t, err)
	result, err := catalog.Handler(t.Context(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: arguments}})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Less(t, len(err.Error()), 256)
}
