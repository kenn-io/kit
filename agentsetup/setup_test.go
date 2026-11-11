package agentsetup

import (
	"bufio"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"go.kenn.io/kit/internal/codexapp/testserver"
)

func TestMain(m *testing.M) {
	if os.Getenv("SETUP_HELPER") != "" {
		os.Exit(runHelper())
	}
	os.Exit(m.Run())
}

func runHelper() int {
	if len(os.Args) == 5 && os.Args[1] == "mcp" && os.Args[2] == "get" && os.Args[3] == "--" && os.Args[4] == "example" {
		fmt.Print("Scope: User config\nStatus: Connected\n")
		return 0
	}
	input := bufio.NewScanner(os.Stdin)
	if testserver.Handshake(input, os.Stdout) != nil {
		return 2
	}
	for {
		request, err := testserver.Read(input)
		if errors.Is(err, io.EOF) {
			return 0
		}
		if err != nil {
			return 3
		}
		switch request.Method {
		case "config/read":
			if testserver.CheckConfigRead(request) != nil {
				return 5
			}
			_ = testserver.Reply(os.Stdout, request, `{"config":{"features":{"hooks":true}}}`)
		case "hooks/list":
			if testserver.CheckHooksList(request) != nil {
				return 6
			}
			path := filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json")
			trust := os.Getenv("SETUP_TRUST")
			if trust == "" {
				trust = "untrusted"
			}
			var hooks []any
			for _, event := range []string{"stop", "sessionstart", "sessionend"} {
				hooks = append(hooks, map[string]any{"source": "user", "sourcePath": path, "key": path + ":" + event + ":0:0", "trustStatus": trust, "enabled": true})
			}
			result := map[string]any{"data": []any{map[string]any{"hooks": hooks, "errors": []any{}}}}
			data, _ := json.Marshal(result)
			_ = testserver.Reply(os.Stdout, request, string(data))
		case "mcpServerStatus/list":
			if os.Getenv("SETUP_MCP_FAILURE") != "" {
				if testserver.CheckMCPList(request, "first") != nil && testserver.CheckMCPList(request, "second") != nil {
					return 7
				}
				_ = testserver.Reply(os.Stdout, request, `{"data":[{"name":"first","toolsError":"failed"},{"name":"second","toolsError":"failed"}]}`)
				continue
			}
			if testserver.CheckMCPList(request, "example") != nil {
				return 7
			}
			data, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"name": "example", "tools": map[string]any{"search": map[string]any{}}, "toolsError": nil}}})
			_ = testserver.Reply(os.Stdout, request, string(data))
		default:
			return 4
		}
	}
}
