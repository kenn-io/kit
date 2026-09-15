package v2helper

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"v2helper/humaconfig"
)

type Server struct{ api huma.API }

func New() *Server {
	mux := http.NewServeMux()
	return &Server{api: humago.NewWithPrefix(mux, "/api/v1", humaconfig.New("helper"))}
}

// NewReconfigured threads a default config through the helper.
func NewReconfigured() huma.API {
	cfg := huma.DefaultConfig("helper", "1")
	cfg.DocsPath = ""
	cfg = humaconfig.Configure(cfg)
	return humago.New(http.NewServeMux(), cfg)
}

// NewViaSink passes the config through a module wrapper; the wrapper is
// not judged, its callers are.
func NewViaSink() huma.API {
	return newRecordingAPI(http.NewServeMux(), "/api", humaconfig.New("sink"))
}

func newRecordingAPI(mux *http.ServeMux, prefix string, config huma.Config) huma.API {
	return huma.NewAPI(config, humago.NewAdapter(mux, prefix))
}

// NewViaSinkDefault is judged at the wrapper call, not inside the wrapper.
func NewViaSinkDefault() huma.API {
	return newRecordingAPI(http.NewServeMux(), "/api", huma.DefaultConfig("sink", "1")) // want "encoding/json \(v1\) formats"
}

func (s *Server) method() huma.Config { return humaconfig.New("method") }

func NewFromMethod(s *Server) huma.API {
	return humago.New(http.NewServeMux(), s.method())
}
