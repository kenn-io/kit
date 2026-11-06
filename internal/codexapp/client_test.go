package codexapp

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("KIT_CODEXAPP_TEST_HELPER"); mode != "" {
		os.Exit(runClientHelper(mode))
	}
	os.Exit(m.Run())
}

func runClientHelper(mode string) int {
	if len(os.Args) != 3 || os.Args[1] != "app-server" || os.Args[2] != "--stdio" {
		return 2
	}
	if file := os.Getenv("KIT_CODEXAPP_TEST_PID"); file != "" {
		if err := os.WriteFile(file, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 3
		}
	}
	input := bufio.NewScanner(os.Stdin)
	if !input.Scan() {
		return 4
	}
	var request struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(input.Bytes(), &request); err != nil {
		return 5
	}
	if request.ID != 1 || request.Method != "initialize" {
		return 6
	}
	info, _ := request.Params["clientInfo"].(map[string]any)
	caps, _ := request.Params["capabilities"].(map[string]any)
	if info["name"] != "kit-agent-check" || caps["experimentalApi"] != true {
		return 7
	}
	if mode == "initialize-eof" {
		return 0
	}
	fmt.Println(`{"method":"notice","params":{}}`)
	fmt.Println(`{"id":999,"result":{}}`)
	fmt.Println(`{"id":1,"result":{}}`)
	if !input.Scan() || string(input.Bytes()) != `{"method":"initialized","params":{}}` {
		return 8
	}
	for input.Scan() {
		if err := json.Unmarshal(input.Bytes(), &request); err != nil {
			return 9
		}
		if mode == "block" {
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, os.Interrupt)
			<-signals
			signal.Stop(signals)
			return 10
		}
		cwd, _ := os.Getwd()
		if request.ID != 2 || request.Method != "config/read" || request.Params["includeLayers"] != false || request.Params["cwd"] != cwd {
			return 11
		}
		fmt.Println(`{"id":2,"result":{"ready":true}}`)
	}
	if mode == "block-close" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt)
		<-signals
		signal.Stop(signals)
	}
	if file := os.Getenv("KIT_CODEXAPP_TEST_EXIT"); file != "" {
		_ = os.WriteFile(file, []byte("EOF"), 0o600)
	}
	return 0
}

func TestClientCancellationAndEOF(t *testing.T) {
	for _, mode := range []string{"normal", "block", "block-close", "initialize-eof"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			require.NoError(t, err)
			dir := t.TempDir()
			pidFile, exitFile := filepath.Join(dir, "pid"), filepath.Join(dir, "exit")
			t.Setenv("KIT_CODEXAPP_TEST_HELPER", mode)
			t.Setenv("KIT_CODEXAPP_TEST_PID", pidFile)
			t.Setenv("KIT_CODEXAPP_TEST_EXIT", exitFile)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			client, startErr := Start(ctx, executable, dir)
			if mode == "initialize-eof" {
				require.Error(t, startErr)
			} else {
				require.NoError(t, startErr)
				var reply struct {
					Ready bool `json:"ready"`
				}
				var callErr error
				if mode == "normal" || mode == "block" {
					callErr = client.Call("config/read", map[string]any{"includeLayers": false, "cwd": dir}, &reply)
				}
				client.Close()
				if mode == "block" {
					require.Error(t, callErr)
				}
				if mode == "normal" {
					require.NoError(t, callErr)
					assert.True(t, reply.Ready)
				}
			}
			if mode == "normal" {
				data, err := os.ReadFile(exitFile)
				require.NoError(t, err)
				assert.Equal(t, "EOF", string(data))
			}
			if runtime.GOOS != "windows" {
				data, err := os.ReadFile(pidFile)
				require.NoError(t, err)
				pid, err := strconv.Atoi(string(data))
				require.NoError(t, err)
				process, err := os.FindProcess(pid)
				require.NoError(t, err)
				require.Error(t, process.Signal(syscall.Signal(0)), "the exact child must be reaped before returning")
				_ = process.Release()
			}
		})
	}
}
