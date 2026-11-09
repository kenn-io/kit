package agentmcp_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "kit-mcp-stdio" {
		server := mcp.NewServer(&mcp.Implementation{Name: "kit-test", Version: "1"}, nil)
		mcp.AddTool(server, &mcp.Tool{Name: "ping", Description: "Test tool"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
		// This standalone fixture runs until the agent closes its input.
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if mode := os.Getenv("KIT_MCP_CHECK_HELPER"); mode != "" {
		os.Exit(runCheckHelper(mode))
	}
	if response := os.Getenv("KIT_MCP_TEST_RESPONSE"); response != "" {
		if len(os.Args) != 4 || os.Args[1] != "mcp" || os.Args[2] != "status" || os.Args[3] != "--json" {
			os.Exit(2)
		}
		fmt.Print(response)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runCheckHelper(mode string) int {
	if strings.HasPrefix(mode, "claude") {
		if len(os.Args) != 5 || strings.Join(os.Args[1:4], " ") != "mcp get --" || os.Args[4] != "example" {
			return 2
		}
		fmt.Print(os.Getenv("KIT_MCP_CHECK_REPLY"))
		if mode == "claude-failed" {
			return 1
		}
		return 0
	}
	input := bufio.NewScanner(os.Stdin)
	if err := testserver.Handshake(input, os.Stdout); err != nil {
		return 3
	}
	request, err := testserver.Read(input)
	if err != nil || request.Method != "mcpServerStatus/list" || request.Params["serverName"] != "example" || len(request.Params) != 1 {
		return 4
	}
	fmt.Printf("{\"id\":%d,\"result\":%s}\n", request.ID, os.Getenv("KIT_MCP_CHECK_REPLY"))
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

func setupInstalledAgent(t *testing.T, agent agentmcp.Agent) {
	t.Helper()
	if os.Getenv("KIT_MCP_LIVE_TEST") != "1" {
		t.Skip("requires installed agents")
	}
	_, err := exec.LookPath(string(agent))
	require.NoError(t, err)
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("CLAUDE_CODE_SAFE_MODE", "")
	t.Chdir(dir)
}
