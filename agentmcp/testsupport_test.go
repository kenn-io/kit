package agentmcp_test

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/agentmcp"
	"go.kenn.io/kit/internal/codexapp/testserver"
)

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
	if err != nil || testserver.CheckMCPList(request, "example") != nil {
		return 4
	}
	_ = testserver.Reply(os.Stdout, request, os.Getenv("KIT_MCP_CHECK_REPLY"))
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
