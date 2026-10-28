package embedlocal

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestEndpointOllamaFallback(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"bare", `{"models":[{"name":"nomic-embed-text"}]}`, 200},
		{"latest", `{"models":[{"name":"nomic-embed-text:latest"}]}`, 200},
		{"status", `{"models":[{"name":"nomic-embed-text:latest"}]}`, 503},
		{"chat only", `{"version":"0.20.0","models":[{"name":"llama3.2:latest"}]}`, 200},
		{"custom tag", `{"models":[{"name":"nomic-embed-text:v1.5"}]}`, 200},
		{"oversized", `{"padding":"` + strings.Repeat("x", 1<<20) + `","version":"0.20.0"}`, 200},
		{"trailing data", `{"models":[{"name":"nomic-embed-text"}]}garbage`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/tags", r.URL.Path)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer ollama.Close()
			dir := t.TempDir()
			want := localServer(t, dir, "1").URL + "/v1"
			if tc.name == "bare" || tc.name == "latest" {
				dir = filepath.Join(dir, "file")
				require.NoError(t, os.WriteFile(dir, nil, 0o600))
				want = ollama.URL + "/v1"
			}
			got, err := endpoint(t.Context(), Options{DataDir: dir}, ollama.URL)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestEndpointMissingAndUnreachable(t *testing.T) {
	ollama := httptest.NewServer(http.NotFoundHandler())
	defer ollama.Close()
	dir := t.TempDir()
	_, err := endpoint(t.Context(), Options{DataDir: dir}, ollama.URL)
	require.ErrorIs(t, err, ErrNotFound)
	localServer(t, dir, "2")
	_, err = endpoint(t.Context(), Options{DataDir: dir}, ollama.URL)
	require.ErrorIs(t, err, ErrNotFound)
	local := localServer(t, dir, "1")
	local.Close()
	_, err = endpoint(t.Context(), Options{DataDir: dir, Executable: "must-not-start"}, ollama.URL)
	require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "exit"), nil, 0o600))
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = endpoint(ctx, Options{DataDir: dir, Executable: executable}, ollama.URL)
	require.ErrorContains(t, err, "ONNX Runtime library missing")
	require.ErrorContains(t, err, filepath.Join(dir, "kennembed.log"))
	assert.NoError(t, ctx.Err(), "child exit should end startup before its timeout")
}

func localServer(t *testing.T, dir, version string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{Service: Service, Version: version}))
	t.Cleanup(server.Close)
	ep, err := daemon.ParseEndpoint(server.URL, daemon.ParseEndpointOptions{TCPPolicy: daemon.RequireLoopback})
	require.NoError(t, err)
	_, err = (daemon.RuntimeStore{Dir: dir, Prefix: Service}).Write(daemon.NewRuntimeRecord(Service, version, ep))
	require.NoError(t, err)
	return server
}

func TestEndpointStartup(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			ollama := httptest.NewServer(http.NotFoundHandler())
			defer ollama.Close()
			dir := t.TempDir()
			entered, release := make(chan int, 1), make(chan struct{})
			openGate := sync.OnceFunc(func() { close(release) })
			gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
				select {
				case entered <- pid:
				default:
				}
				<-release
				w.WriteHeader(status)
			}))
			defer gate.Close()
			defer openGate()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte(gate.URL), 0o600))
			executable, err := os.Executable()
			require.NoError(t, err)
			var wg sync.WaitGroup
			results := make([]struct {
				url string
				err error
			}, 4)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var childPID int
			t.Cleanup(func() {
				if status == 503 {
					require.Eventually(t, func() bool { return !daemon.ProcessAlive(childPID) }, 5*time.Second, 10*time.Millisecond)
					return
				}
				var records []daemon.RuntimeRecord
				var err error
				require.Eventually(t, func() bool {
					records, err = (daemon.RuntimeStore{Dir: dir, Prefix: Service}).List()
					return err == nil && len(records) == 1
				}, 5*time.Second, 10*time.Millisecond)
				assert.Equal(t, childPID, records[0].PID)
				assert.Equal(t, records[0].Endpoint().BaseURL()+"/v1", results[0].url)
				req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodPost, records[0].Endpoint().BaseURL()+"/stop", nil)
				require.NoError(t, err)
				client := &http.Client{Timeout: 5 * time.Second}
				resp, err := client.Do(req)
				require.NoError(t, err)
				resp.Body.Close()
				require.Eventually(t, func() bool { return !daemon.ProcessAlive(records[0].PID) }, 5*time.Second, 10*time.Millisecond)
			})
			first := make(chan error, 1)
			go func() {
				_, err := endpoint(ctx, Options{DataDir: dir, Executable: executable}, ollama.URL)
				first <- err
			}()
			childPID = <-entered
			cancel()
			firstErr := <-first
			require.ErrorIs(t, firstErr, context.Canceled)
			require.ErrorContains(t, firstErr, filepath.Join(dir, "kennembed.log"))
			joining, stopJoining := context.WithTimeout(t.Context(), 5*time.Second)
			defer stopJoining()
			for i := range results {
				wg.Go(func() {
					results[i].url, results[i].err = endpoint(joining, Options{DataDir: dir, Executable: executable}, ollama.URL)
				})
			}
			store := daemon.RuntimeStore{Dir: dir, Prefix: Service}
			require.Eventually(t, func() bool {
				unlock, acquired, err := store.TryAcquireStartLock(t.Context())
				if acquired {
					unlock()
				}
				return err == nil && !acquired
			}, 5*time.Second, time.Millisecond)
			waiting, stopWaiting := context.WithTimeout(t.Context(), 3*time.Second)
			unlock, err := store.AcquireStartLock(waiting)
			stopWaiting()
			if unlock != nil {
				unlock()
			}
			require.ErrorIs(t, err, context.DeadlineExceeded, "callers must keep waiting while the owner loads")
			openGate()
			wg.Wait()
			for _, result := range results {
				if status == 503 {
					require.ErrorContains(t, result.err, "cache model.onnx: download: HTTP 503")
				} else {
					require.NoError(t, result.err)
					assert.Equal(t, results[0].url, result.url)
				}
			}
			assert.NoError(t, joining.Err(), "shared failure must be reported before the caller deadline")
		})
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-data-dir" {
		os.Exit(runServer(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runServer(args []string) int {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	dir := flags.String("data-dir", "", "")
	listen := flags.String("listen", "", "")
	if flags.Parse(args) != nil || !filepath.IsAbs(*dir) || *listen != "127.0.0.1:0" {
		return 2
	}
	store := daemon.RuntimeStore{Dir: *dir, Prefix: Service}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := store.AcquireOwnerLock(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 11
	}
	defer release()
	if _, err := os.Stat(filepath.Join(*dir, "exit")); err == nil {
		fmt.Fprintln(os.Stderr, "ONNX Runtime library missing")
		return 12
	}
	if gate, err := os.ReadFile(filepath.Join(*dir, "gate")); err == nil {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, fmt.Sprintf("%s?pid=%d", gate, os.Getpid()), nil)
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return 8
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintln(os.Stderr, "cache model.onnx: download: HTTP 503")
			return 8
		}
	}
	listener, err := new(net.ListenConfig).Listen(context.Background(), "tcp", *listen)
	if err != nil {
		return 5
	}
	defer listener.Close()
	mux := http.NewServeMux()
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: Service, Version: "1"}))
	stop := make(chan struct{})
	mux.HandleFunc("POST /stop", func(http.ResponseWriter, *http.Request) { close(stop) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	_, err = (daemon.RuntimeStore{Dir: *dir, Prefix: Service}).Write(daemon.NewRuntimeRecord(Service, "1", daemon.Endpoint{Network: daemon.NetworkTCP, Address: listener.Addr().String()}))
	if err != nil {
		return 6
	}
	<-stop
	return 0
}
