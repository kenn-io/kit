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
			fmt.Printf("{\"id\":%d,\"result\":{\"config\":{\"features\":{\"hooks\":true}}}}\n", request.ID)
		case "hooks/list":
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
			fmt.Printf("{\"id\":%d,\"result\":%s}\n", request.ID, data)
		case "mcpServerStatus/list":
			if os.Getenv("SETUP_MCP_FAILURE") != "" {
				fmt.Printf("{\"id\":%d,\"result\":{\"data\":[{\"name\":\"first\",\"toolsError\":\"failed\"},{\"name\":\"second\",\"toolsError\":\"failed\"}]}}\n", request.ID)
				continue
			}
			data, _ := json.Marshal(map[string]any{"data": []any{map[string]any{"name": "example", "tools": map[string]any{"search": map[string]any{}}, "toolsError": nil}}})
			fmt.Printf("{\"id\":%d,\"result\":%s}\n", request.ID, data)
		default:
			return 4
		}
	}
}
