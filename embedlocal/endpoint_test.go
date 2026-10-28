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
		backend    string
	}{
		{"latest", `{"models":[{"name":"nomic-embed-text:latest"}]}`, 200, "ollama"},
		{"status", `{"models":[{"name":"nomic-embed-text:latest"}]}`, 503, "local"},
		{"custom tag", `{"models":[{"name":"nomic-embed-text:v1.5"}]}`, 200, "local"},
		{"oversized", `{"padding":"` + strings.Repeat("x", 1<<20) + `","models":[{"name":"nomic-embed-text:latest"}]}`, 200, "local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/tags", r.URL.Path)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer ollama.Close()
			dir := t.TempDir()
			backends := map[string]string{"local": localServer(t, dir, Version).URL + "/v1", "ollama": ollama.URL + "/v1"}
			got, err := endpoint(t.Context(), Options{DataDir: dir}, ollama.URL, 2*time.Minute)
			require.NoError(t, err)
			assert.Equal(t, backends[tc.backend], got)
		})
	}
}

func TestEndpointMissingAndUnreachable(t *testing.T) {
	ollama := httptest.NewServer(http.NotFoundHandler())
	defer ollama.Close()
	dir := t.TempDir()
	_, err := endpoint(t.Context(), Options{DataDir: dir}, ollama.URL, 2*time.Minute)
	require.ErrorIs(t, err, ErrNotFound)
	localServer(t, dir, "2")
	_, err = endpoint(t.Context(), Options{DataDir: dir}, ollama.URL, 2*time.Minute)
	require.ErrorIs(t, err, ErrNotFound)
	local := localServer(t, dir, Version)
	local.Close()
	_, err = endpoint(t.Context(), Options{DataDir: dir, Executable: "must-not-start"}, ollama.URL, 2*time.Minute)
	require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
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
	for _, tc := range []struct {
		name   string
		status int
		delay  time.Duration
	}{
		{"reuse", 200, 0}, {"failed", 503, 0}, {"early", 0, 0}, {"slow", 200, 200 * time.Millisecond}, {"default", 200, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
				w.WriteHeader(tc.status)
			}))
			defer gate.Close()
			defer openGate()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "gate"), []byte(gate.URL), 0o600))
			if tc.name == "early" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "exit"), nil, 0o600))
			}
			executable, err := os.Executable()
			require.NoError(t, err)
			type result struct {
				url string
				err error
			}
			results := make([]result, 4)
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.name == "default" {
				ctx, cancel = context.WithCancel(context.WithoutCancel(t.Context()))
			} else {
				ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
			}
			defer cancel()
			var childPID int
			t.Cleanup(func() {
				launches, err := filepath.Glob(filepath.Join(dir, "launch.*"))
				require.NoError(t, err)
				for _, launch := range launches {
					pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(launch), "launch."))
					require.NoError(t, err)
					require.Eventually(t, func() bool { return !daemon.ProcessAlive(pid) }, 5*time.Second, 10*time.Millisecond)
				}
			})
			t.Cleanup(func() {
				if tc.name == "early" {
					return
				}
				if tc.status == 503 {
					require.Eventually(t, func() bool { return !daemon.ProcessAlive(childPID) }, 5*time.Second, 10*time.Millisecond)
					return
				}
				var records []daemon.RuntimeRecord
				require.Eventually(t, func() bool {
					records, err = (daemon.RuntimeStore{Dir: dir, Prefix: Service}).List()
					return err == nil && len(records) == 1
				}, 5*time.Second, 10*time.Millisecond)
				req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), http.MethodPost, records[0].Endpoint().BaseURL()+"/stop", nil)
				require.NoError(t, err)
				resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
				require.NoError(t, err)
				resp.Body.Close()
				require.Eventually(t, func() bool { return !daemon.ProcessAlive(records[0].PID) }, 5*time.Second, 10*time.Millisecond)
			})
			first := make(chan result, 1)
			go func() {
				url, err := endpoint(ctx, Options{DataDir: dir, Executable: executable}, ollama.URL, 100*time.Millisecond)
				first <- result{url, err}
			}()
			if tc.name == "early" {
				err := (<-first).err
				require.ErrorContains(t, err, "ONNX Runtime library missing")
				require.ErrorContains(t, err, filepath.Join(dir, "kennembed.log"))
				assert.NoError(t, ctx.Err(), "child exit should end startup before its timeout")
				return
			}
			childPID = <-entered
			if tc.delay > 0 {
				time.Sleep(tc.delay) //nolint:kennlint // separates the default budget from a longer caller deadline across the child process
				openGate()
				results = []result{<-first}
				if tc.name == "default" {
					require.ErrorIs(t, results[0].err, context.DeadlineExceeded)
					require.ErrorContains(t, results[0].err, filepath.Join(dir, "kennembed.log"))
					return
				}
			} else {
				cancel()
				firstErr := (<-first).err
				require.ErrorIs(t, firstErr, context.Canceled)
				require.ErrorContains(t, firstErr, filepath.Join(dir, "kennembed.log"))
				joining, stopJoining := context.WithTimeout(t.Context(), 5*time.Second)
				defer stopJoining()
				var wg sync.WaitGroup
				for i := range results {
					wg.Go(func() {
						results[i].url, results[i].err = endpoint(joining, Options{DataDir: dir, Executable: executable}, ollama.URL, 100*time.Millisecond)
					})
				}
				store := daemon.RuntimeStore{Dir: dir, Prefix: Service}
				waitForStartup(t, store)
				assert.Never(t, func() bool {
					launches, err := filepath.Glob(filepath.Join(dir, "launch.*"))
					return err != nil || len(launches) != 1
				}, time.Second, 10*time.Millisecond, "loading must not launch another child")
				openGate()
				wg.Wait()
				launches, err := filepath.Glob(filepath.Join(dir, "launch.*"))
				require.NoError(t, err)
				assert.Len(t, launches, 1, "joining callers must reuse the original child")
				require.NoError(t, joining.Err(), "shared failure must be reported before the caller deadline")
			}
			for _, result := range results {
				if tc.status == 503 {
					require.ErrorContains(t, result.err, "cache model.onnx: download: HTTP 503")
				} else {
					require.NoError(t, result.err)
					if tc.delay == 0 {
						assert.Equal(t, results[0].url, result.url)
					}
				}
			}
			if tc.status == 200 {
				records, err := (daemon.RuntimeStore{Dir: dir, Prefix: Service}).List()
				require.NoError(t, err)
				require.Len(t, records, 1)
				assert.Equal(t, childPID, records[0].PID)
				assert.Equal(t, records[0].Endpoint().BaseURL()+"/v1", results[0].url)
			}
		})
	}
}

func waitForStartup(t *testing.T, store daemon.RuntimeStore) {
	t.Helper()
	require.Eventually(t, func() bool {
		unlock, acquired, err := store.TryAcquireStartLock(t.Context())
		if acquired {
			unlock()
		}
		return err == nil && !acquired
	}, 5*time.Second, time.Millisecond)
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
	if err := os.WriteFile(filepath.Join(*dir, fmt.Sprintf("launch.%d", os.Getpid())), nil, 0o600); err != nil {
		return 3
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
		client := &http.Client{Timeout: 3 * time.Minute}
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
	mux.Handle("/api/ping", daemon.NewPingHandler(daemon.PingHandlerOptions{Service: Service, Version: Version}))
	stop := make(chan struct{})
	mux.HandleFunc("POST /stop", func(http.ResponseWriter, *http.Request) { close(stop) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() { server.Close(); <-done }()
	_, err = (daemon.RuntimeStore{Dir: *dir, Prefix: Service}).Write(daemon.NewRuntimeRecord(Service, Version, daemon.Endpoint{Network: daemon.NetworkTCP, Address: listener.Addr().String()}))
	if err != nil {
		return 6
	}
	<-stop
	return 0
}
