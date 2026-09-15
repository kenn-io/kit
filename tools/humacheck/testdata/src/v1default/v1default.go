package v1default

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func New() huma.API {
	return humago.New(http.NewServeMux(), huma.DefaultConfig("default", "1")) // want "encoding/json \(v1\) formats"
}

func NewTweaked() huma.API {
	cfg := huma.DefaultConfig("default", "1")
	cfg.OpenAPIPath = ""
	cfg.DocsPath = ""
	return humago.New(http.NewServeMux(), cfg) // want "encoding/json \(v1\) formats"
}

func config() huma.Config {
	return huma.DefaultConfig("default", "1")
}

func NewFromFunc() huma.API {
	return huma.NewAPI(config(), humago.NewAdapter(http.NewServeMux(), "")) // want "encoding/json \(v1\) formats"
}
