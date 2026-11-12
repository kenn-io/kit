package mcpskills_test

import (
	"context"
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
			Name:     "workflow",
			Title:    "Workflow",
			Summary:  "Start here",
			Markdown: bundledWorkflow + "\nUse " + searchTool + " to find items.\n",
		}},
	})
	if err != nil {
		return err
	}
	listed, err := catalog.Handler(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}})
	if err != nil {
		return err
	}
	read, err := catalog.Handler(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{"name":"workflow"}`)}})
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n%s", toolName, listed.StructuredContent.(mcpskills.Output).Skills[0].Title, read.Content[0].(*mcp.TextContent).Text)
	return nil
}
