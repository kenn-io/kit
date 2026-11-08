// Package mcphttp mounts authenticated, stateless MCP on an application's HTTP
// server. The application owns its listener, credentials, tools and stdio path.
package mcphttp

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Options configures an endpoint. There is no unauthenticated default.
type Options struct {
	// Authenticate is the application's authentication middleware. It must reject
	// unauthorized requests before calling next and may attach identity to the
	// request context. Include the application's host and authorization policy
	// here too. It must not remove request cancellation.
	Authenticate func(next http.Handler) http.Handler

	// MaxRequestBodyBytes bounds incoming messages. Zero uses the SDK default;
	// negative values are rejected.
	MaxRequestBodyBytes int64
}

// Handler serves stateless JSON responses with no retained protocol sessions.
// Construct it with New and call Shutdown before releasing tool dependencies.
type Handler struct {
	handler http.Handler
	cancel  context.CancelFunc

	mu      sync.Mutex
	closing bool
	active  int
	drained chan struct{}
}

// New builds an endpoint to mount at /mcp in an existing mux. newServer must
// return a fresh server for each request: Kit installs request cancellation
// middleware on it. Capture authenticated identity and per-request tool policy
// from r; do not mutate or return a shared server used by HTTP or stdio.
// Returning nil rejects the request using the SDK's bad-request response.
//
// Stateless HTTP cannot send server-initiated requests or retain negotiated
// session state. Applications requiring these should use the SDK directly.
func New(newServer func(r *http.Request) *mcp.Server, opts Options) (*Handler, error) {
	if newServer == nil {
		return nil, errors.New("MCP HTTP server factory is required")
	}
	if opts.Authenticate == nil {
		return nil, errors.New("MCP HTTP authentication middleware is required")
	}
	if opts.MaxRequestBodyBytes < 0 {
		return nil, errors.New("MCP HTTP request body limit must not be negative")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	transport := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		// The SDK may ask for the server more than once per request.
		state := r.Context().Value(serverKey{}).(*requestServer)
		state.once.Do(func() {
			state.server = newServer(r)
			if state.server != nil {
				state.server.AddReceivingMiddleware(requestCancellation(r.Context()))
			}
		})
		return state.server
	}, &mcp.StreamableHTTPOptions{
		Stateless:                    true,
		JSONResponse:                 true,
		PropagateRequestCancellation: true,
		MaxRequestBodyBytes:          opts.MaxRequestBodyBytes,
	})
	// Keep the SDK's localhost/DNS-rebinding protection and reject cross-origin
	// browser writes. Trusted reverse-proxy host policy belongs to the caller.
	protected := opts.Authenticate(http.NewCrossOriginProtection().Handler(transport))
	if protected == nil {
		cancel()
		return nil, errors.New("MCP HTTP authentication middleware returned nil")
	}
	h := &Handler{cancel: cancel, drained: make(chan struct{})}
	h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, stop := context.WithCancel(r.Context())
		stopLifetime := context.AfterFunc(lifetime, stop)
		defer stopLifetime()
		defer stop()
		if lifetime.Err() != nil {
			stop()
		}
		ctx = context.WithValue(ctx, serverKey{}, &requestServer{})
		protected.ServeHTTP(w, r.WithContext(ctx))
	})
	return h, nil
}

// ServeHTTP implements http.Handler. Authentication failures, protocol replies
// and draining responses all carry Cache-Control: no-store.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		http.Error(w, "MCP endpoint is shutting down", http.StatusServiceUnavailable)
		return
	}
	h.active++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.active--
		if h.closing && h.active == 0 {
			close(h.drained)
		}
	}()
	h.handler.ServeHTTP(noStoreWriter{w}, r)
}

type serverKey struct{}

type requestServer struct {
	once   sync.Once
	server *mcp.Server
}

type noStoreWriter struct{ http.ResponseWriter }

func (w noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w noStoreWriter) WriteHeader(status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(status)
}

func (w noStoreWriter) Write(body []byte) (int, error) {
	w.Header().Set("Cache-Control", "no-store")
	return w.ResponseWriter.Write(body)
}

// Shutdown stops admission and waits for in-flight requests. When ctx expires,
// it cancels their tool contexts and returns ctx.Err(). Tool handlers must honor
// cancellation; Shutdown cannot forcibly stop application goroutines. It is safe
// to call concurrently or repeatedly. It does not shut down the HTTP listener.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	if !h.closing {
		h.closing = true
		if h.active == 0 {
			close(h.drained)
		}
	}
	h.mu.Unlock()
	select {
	case <-h.drained:
		h.cancel()
		return nil
	case <-ctx.Done():
		h.cancel()
		return ctx.Err()
	}
}

// The SDK intentionally detaches some method contexts from HTTP cancellation
// for older protocols. Stateless JSON has no resumable response, so join both
// lifetimes without losing SDK context values or MCP cancellation.
func requestCancellation(requestContext context.Context) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			ctx, cancel := context.WithCancel(ctx)
			stop := context.AfterFunc(requestContext, cancel)
			defer stop()
			defer cancel()
			if requestContext.Err() != nil {
				cancel()
			}
			return next(ctx, method, req)
		}
	}
}
