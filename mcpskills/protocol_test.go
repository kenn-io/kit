package mcpskills_test

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/mcpskills"
)

func TestTwoConsumersOverMCP(t *testing.T) {
	for _, name := range []string{"alpha.skills", "beta_guides"} {
		t.Run(name, func(t *testing.T) {
			config := fixture()
			config.ToolName = name
			config.Guides[0].Markdown = " \r\n# Guide café\r\n\n```json\n{\"quoted\": \"\\\\\"}\n```\n\t"
			expectedEntries := []mcpskills.Entry{
				{Name: "workflow", Title: "Workflow", Summary: "Start here"},
				{Name: "search", Title: "Search", Summary: "Find things"},
			}
			catalog, err := mcpskills.New(config)
			require.NoError(t, err)
			server := mcp.NewServer(&mcp.Implementation{Name: name, Version: "test"}, &mcp.ServerOptions{Instructions: "Existing instructions. " + catalog.Instructions()})
			if name == "alpha.skills" {
				catalog.Register(server)
			} else {
				// A consumer with a central catalog retains control of registration.
				server.AddTool(catalog.Tool(), catalog.Handler)
			}
			serverTransport, clientTransport := mcp.NewInMemoryTransports()
			serverSession, err := server.Connect(t.Context(), serverTransport, nil)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, serverSession.Close()) })
			client := mcp.NewClient(&mcp.Implementation{Name: "fixture", Version: "test"}, nil)
			session, err := client.Connect(t.Context(), clientTransport, nil)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, session.Close()) })
			assert.Contains(t, session.InitializeResult().Instructions, "Existing instructions.")
			assert.Contains(t, session.InitializeResult().Instructions, name)
			tools, err := session.ListTools(t.Context(), nil)
			require.NoError(t, err)
			require.Len(t, tools.Tools, 1)
			assert.Equal(t, name, tools.Tools[0].Name)
			assert.True(t, tools.Tools[0].Annotations.ReadOnlyHint)
			assert.True(t, tools.Tools[0].Annotations.IdempotentHint)
			require.NotNil(t, tools.Tools[0].Annotations.OpenWorldHint)
			assert.False(t, *tools.Tools[0].Annotations.OpenWorldHint)
			inputSchema := resolveSchema(t, tools.Tools[0].InputSchema)
			require.NoError(t, inputSchema.Validate(map[string]any{}))
			require.NoError(t, inputSchema.Validate(map[string]any{"name": "workflow"}))
			require.Error(t, inputSchema.Validate(map[string]any{"name": ""}))
			require.Error(t, inputSchema.Validate(map[string]any{"name": nil}))
			require.Error(t, inputSchema.Validate(map[string]any{"unknown": "workflow"}))
			outputSchema := resolveSchema(t, tools.Tools[0].OutputSchema)
			listed, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
			require.NoError(t, err)
			assert.Equal(t, expectedEntries, metadata(t, listed).Skills)
			require.NoError(t, outputSchema.Validate(listed.StructuredContent))
			read, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: map[string]string{"name": "workflow"}})
			require.NoError(t, err)
			assert.Equal(t, config.Guides[0].Markdown, read.Content[0].(*mcp.TextContent).Text)
			assert.Equal(t, []mcpskills.Entry{config.Guides[0].Entry}, metadata(t, read).Skills)
			require.NoError(t, outputSchema.Validate(read.StructuredContent))
			for _, arguments := range []json.RawMessage{json.RawMessage(`{"name":"missing"}`), json.RawMessage(`{"name":null}`), json.RawMessage(`{"other":true}`), json.RawMessage(`{"name":"workflow","name":"workflow"}`)} {
				_, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: arguments})
				var rpcError *jsonrpc.Error
				require.ErrorAs(t, err, &rpcError)
				assert.EqualValues(t, jsonrpc.CodeInvalidParams, rpcError.Code)
			}
		})
	}
}

func resolveSchema(t *testing.T, value any) *jsonschema.Resolved {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal(data, &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	return resolved
}
