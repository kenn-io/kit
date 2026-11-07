package codexapp

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"errors"
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
	"go.kenn.io/kit/consolewindow"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("KIT_CODEXAPP_TEST_HELPER"); mode != "" {
		os.Exit(runClientHelper(mode))
	}
	os.Exit(m.Run())
}

func runClientHelper(mode string) int {
	if mode == "hold-pipes" {
		<-time.After(10 * time.Second)
		return 0
	}
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
	if mode == "block" {
		// A launcher can exit while a descendant retains its stdout pipe.
		child := consolewindow.CommandContext(context.Background(), os.Args[0], "app-server", "--stdio")
		child.Env = append(os.Environ(), "KIT_CODEXAPP_TEST_HELPER=hold-pipes")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			return 12
		}
		pid := []byte(strconv.Itoa(child.Process.Pid))
		if err := os.WriteFile(os.Getenv("KIT_CODEXAPP_TEST_DESCENDANT"), pid, 0o600); err != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			return 13
		}
		_ = child.Process.Release()
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
		if request.ID != 2 || request.Method != "example/read" || request.Params["example"] != "value" {
			return 11
		}
		if mode == "rpc-error" {
			fmt.Println(`{"id":2,"error":{"message":"native failure"}}`)
			continue
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
	for _, mode := range []string{"normal", "rpc-error", "block", "block-close", "initialize-eof"} {
		t.Run(mode, func(t *testing.T) {
			executable, err := os.Executable()
			require.NoError(t, err)
			dir := t.TempDir()
			pidFile, exitFile := filepath.Join(dir, "pid"), filepath.Join(dir, "exit")
			t.Setenv("KIT_CODEXAPP_TEST_HELPER", mode)
			t.Setenv("KIT_CODEXAPP_TEST_PID", pidFile)
			t.Setenv("KIT_CODEXAPP_TEST_EXIT", exitFile)
			if mode == "block" {
				descendantFile := filepath.Join(dir, "descendant")
				t.Setenv("KIT_CODEXAPP_TEST_DESCENDANT", descendantFile)
				t.Cleanup(func() {
					data, err := os.ReadFile(descendantFile)
					require.NoError(t, err)
					pid, err := strconv.Atoi(string(data))
					require.NoError(t, err)
					child, err := os.FindProcess(pid)
					require.NoError(t, err)
					if err := child.Kill(); !errors.Is(err, os.ErrProcessDone) {
						assert.NoError(t, err)
					}
					_ = child.Release()
				})
			}
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
				started := time.Now()
				if mode == "normal" || mode == "rpc-error" || mode == "block" {
					callErr = client.Call("example/read", map[string]any{"example": "value"}, &reply)
				}
				client.Close()
				if mode == "block" {
					require.ErrorIs(t, callErr, context.DeadlineExceeded)
					assert.Less(t, time.Since(started), 3*time.Second, "cancellation must interrupt a read while the descendant holds stdout")
				}
				if mode == "normal" {
					require.NoError(t, callErr)
					assert.True(t, reply.Ready)
				}
				if mode == "rpc-error" {
					require.ErrorContains(t, callErr, "native failure")
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
