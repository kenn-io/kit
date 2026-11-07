package mcpskills_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/kit/mcpskills"
)

func ExampleCatalog() {
	ctx := context.Background()
	for _, toolName := range []string{"alpha.skills", "beta.skills"} {
		if err := exampleConsumer(ctx, toolName); err != nil {
			panic(err)
		}
	}
	// Output:
	// alpha.skills: Workflow
	// # Workflow
	//
	// Use alpha.search to find items.
	// beta.skills: Workflow
	// # Workflow
	//
	// Use beta.search to find items.
}

func exampleConsumer(ctx context.Context, toolName string) error {
	searchTool := "alpha.search"
	if toolName == "beta.skills" {
		searchTool = "beta.search"
	}
	// Reuse the application's bundled guide, then add its MCP adaptation.
	const bundledWorkflow = "# Workflow\n"
	catalog, err := mcpskills.New(mcpskills.Config{
		ToolName: toolName,
		Guides: []mcpskills.Guide{{
			Entry:    {Name: "workflow", Title: "Workflow", Summary: "Start here"},
			Markdown: bundledWorkflow + "\nUse " + searchTool + " to find items.\n",
		}},
	})
	if err != nil {
		return err
	}
	server := mcp.NewServer(&mcp.Implementation{Name: toolName, Version: "1"}, &mcp.ServerOptions{Instructions: "Keep existing server guidance.\n\n" + catalog.Instructions()})
	if toolName == "alpha.skills" {
		catalog.Register(server)
	} else {
		server.AddTool(catalog.Tool(), catalog.Handler) // Central catalog integration.
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		return err
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "example", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	listed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tools.Tools[0].Name, Arguments: map[string]any{}})
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(listed.StructuredContent)
	if err != nil {
		return err
	}
	var metadata mcpskills.Output
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return err
	}
	read, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tools.Tools[0].Name, Arguments: map[string]string{"name": "workflow"}})
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n%s", tools.Tools[0].Name, metadata.Skills[0].Title, read.Content[0].(*mcp.TextContent).Text)
	return nil
}
