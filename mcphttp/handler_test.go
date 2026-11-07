package mcphttp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type identityKey struct{}

func testAuthentication(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := r.Header.Get("Authorization")
		if principal != "Bearer alice" && principal != "Bearer bob" {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, principal)))
	})
}

func testServer(r *http.Request) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	principal, _ := r.Context().Value(identityKey{}).(string)
	mcp.AddTool(s, &mcp.Tool{Name: "identity", Description: "Return synthetic caller"},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: principal}}}, nil, nil
		})
	return s
}

func testHandler(t *testing.T, factory func(*http.Request) *mcp.Server, limit int64) *Handler {
	t.Helper()
	h, err := New(factory, Options{Authenticate: testAuthentication, MaxRequestBodyBytes: limit})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Detach cleanup from test cancellation, then bound draining.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	})
	return h
}

func rpcRequest(t *testing.T, ctx context.Context, endpoint, version, principal, body string) *http.Request {
	t.Helper()
	if version >= "2026-07-28" {
		var message map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &message))
		params, ok := message["params"].(map[string]any)
		require.True(t, ok)
		params["_meta"] = map[string]any{
			mcp.MetaKeyProtocolVersion:    version,
			mcp.MetaKeyClientInfo:         map[string]any{"name": "fixture-client", "version": "1"},
			mcp.MetaKeyClientCapabilities: map[string]any{},
		}
		encoded, err := json.Marshal(message)
		require.NoError(t, err)
		body = string(encoded)
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Protocol-Version", version)
	r.Header.Set("Authorization", principal)
	if version >= "2026-07-28" {
		r.Header.Set("Mcp-Method", "tools/call")
		r.Header.Set("Mcp-Name", "identity")
	}
	return r
}

const identityCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"identity","arguments":{}}}`

func TestNewRequiresExplicitPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func(*http.Request) *mcp.Server
		opts    Options
	}{
		{"factory", nil, Options{Authenticate: testAuthentication}},
		{"authentication", testServer, Options{}},
		{"body limit", testServer, Options{Authenticate: testAuthentication, MaxRequestBodyBytes: -1}},
		{"nil middleware", testServer, Options{Authenticate: func(http.Handler) http.Handler { return nil }}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := New(tc.factory, tc.opts)
			require.Error(t, err)
			assert.Nil(t, h)
		})
	}
}

func TestAuthenticationBeforeProtocolForEveryMethod(t *testing.T) {
	var factories atomic.Int32
	h := testHandler(t, func(r *http.Request) *mcp.Server {
		factories.Add(1)
		return testServer(r)
	}, 0)
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, "/mcp", nil))
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Equal(t, "Bearer", w.Header().Get("WWW-Authenticate"))
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		})
	}
	assert.Zero(t, factories.Load())
}

func TestOriginHostAndBodyLimits(t *testing.T) {
	h := testHandler(t, testServer, 256)
	for _, tc := range []struct {
		name string
		edit func(*http.Request)
		body string
		code int
	}{
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://foreign.example") }, identityCall, http.StatusForbidden},
		{"foreign fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, identityCall, http.StatusForbidden},
		{"same origin", func(r *http.Request) { r.Header.Set("Origin", "http://localhost") }, identityCall, http.StatusOK},
		{"rebinding", func(r *http.Request) {
			r.Host = "foreign.example"
			*r = *r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}))
		}, identityCall, http.StatusForbidden},
		{"oversized", func(*http.Request) {}, strings.Repeat(" ", 257), http.StatusRequestEntityTooLarge},
		{"chunked oversized", func(r *http.Request) { r.ContentLength = -1; r.TransferEncoding = []string{"chunked"} }, strings.Repeat(" ", 257), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := rpcRequest(t, t.Context(), "http://localhost/mcp", "2025-11-25", "Bearer alice", tc.body)
			tc.edit(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assert.Equal(t, tc.code, w.Code, w.Body.String())
			assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
		})
	}
}

func TestMountAndIdentityIsolation(t *testing.T) {
	var factories atomic.Int32
	h := testHandler(t, func(r *http.Request) *mcp.Server {
		factories.Add(1)
		return testServer(r)
	}, 0)
	mux := http.NewServeMux()
	mux.Handle("/mcp", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	for _, principal := range []string{"Bearer alice", "Bearer bob", "Bearer alice"} {
		r := rpcRequest(t, t.Context(), srv.URL+"/mcp", "2025-11-25", principal, identityCall)
		// A caller-supplied session ID must never recover another caller's state.
		r.Header.Set("Mcp-Session-Id", "shared-untrusted-id")
		resp, err := srv.Client().Do(r)
		require.NoError(t, err)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, resp.Body.Close())
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
		var reply struct {
			Result struct {
				Content []struct{ Type, Text string }
			}
		}
		require.NoError(t, json.Unmarshal(body, &reply))
		require.Len(t, reply.Result.Content, 1)
		assert.Equal(t, principal, reply.Result.Content[0].Text)
		assert.Empty(t, resp.Header.Get("Mcp-Session-Id"))
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
	}
	assert.Equal(t, int32(3), factories.Load(), "construct once per HTTP request")
	healthRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/healthz", nil)
	require.NoError(t, err)
	resp, err := srv.Client().Do(healthRequest)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestInitializeAndStatelessMethods(t *testing.T) {
	h := testHandler(t, testServer, 0)
	body := `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture-client","version":"1"}}}`
	r := rpcRequest(t, t.Context(), "http://localhost/mcp", "2025-11-25", "Bearer alice", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Empty(t, w.Header().Get("Mcp-Session-Id"))
	var reply struct {
		ID     int
		Result struct {
			ProtocolVersion string
			ServerInfo      struct{ Name string }
		}
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &reply))
	assert.Equal(t, 7, reply.ID)
	assert.Equal(t, "2025-11-25", reply.Result.ProtocolVersion)
	assert.Equal(t, "fixture", reply.Result.ServerInfo.Name)
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		r := httptest.NewRequestWithContext(t.Context(), method, "/mcp", nil)
		r.Header.Set("Authorization", "Bearer alice")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
		assert.Equal(t, "POST", w.Header().Get("Allow"))
	}
}

func TestCancellationAndForcedShutdown(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		for _, source := range []string{"request", "shutdown"} {
			t.Run(version+"/"+source, func(t *testing.T) {
				started := make(chan struct{})
				cancelled := make(chan struct{})
				h := testHandler(t, func(*http.Request) *mcp.Server {
					s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
					mcp.AddTool(s, &mcp.Tool{Name: "identity", Description: "Wait for cancellation"},
						func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
							close(started)
							<-ctx.Done()
							close(cancelled)
							return nil, nil, ctx.Err()
						})
					return s
				}, 0)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				r := rpcRequest(t, ctx, "http://localhost/mcp", version, "Bearer alice", identityCall)
				done := make(chan struct{})
				go func() {
					defer close(done)
					h.ServeHTTP(httptest.NewRecorder(), r)
				}()
				await(t, started)
				if source == "request" {
					cancel()
				} else {
					deadline, stop := context.WithCancel(t.Context())
					stop()
					require.ErrorIs(t, h.Shutdown(deadline), context.Canceled)
				}
				await(t, cancelled)
				await(t, done)
			})
		}
	}
}

func await(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for handler")
	}
}

func TestGracefulShutdownDrainsAndStopsAdmission(t *testing.T) {
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	// Hold an admitted request in caller authentication to exercise the entire
	// endpoint lifetime, including middleware outside the MCP transport.
	h, err := New(testServer, Options{Authenticate: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			testAuthentication(next).ServeHTTP(w, r)
		})
	}})
	require.NoError(t, err)
	t.Cleanup(func() {
		unblock()
		// Detach cleanup from test cancellation, then bound draining.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		assert.NoError(t, h.Shutdown(ctx))
	})
	r := rpcRequest(t, t.Context(), "http://localhost/mcp", "2025-11-25", "Bearer alice", identityCall)
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	await(t, started)
	shutdown := make(chan error, 1)
	go func() { shutdown <- h.Shutdown(t.Context()) }()
	require.Eventually(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.closing
	}, 5*time.Second, time.Millisecond)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", nil))
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	select {
	case err := <-shutdown:
		assert.Fail(t, "shutdown returned before draining", "%v", err)
	default:
	}
	unblock()
	await(t, done)
	select {
	case err := <-shutdown:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "shutdown did not complete")
	}
	require.NoError(t, h.Shutdown(t.Context()))
}

func TestCancellationPreservesSDKValuesAndCancellation(t *testing.T) {
	type sdkKey struct{}
	for _, source := range []string{"request", "sdk"} {
		t.Run(source, func(t *testing.T) {
			request, stopRequest := context.WithCancel(t.Context())
			defer stopRequest()
			sdk, stopSDK := context.WithCancel(context.WithValue(t.Context(), sdkKey{}, "preserved"))
			defer stopSDK()
			if source == "request" {
				stopRequest()
			} else {
				stopSDK()
			}
			method := requestCancellation(request)(func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
				assert.Equal(t, "preserved", ctx.Value(sdkKey{}))
				return nil, ctx.Err()
			})
			_, err := method(sdk, "tools/call", nil)
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}
