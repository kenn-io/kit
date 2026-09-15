package v1formats

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func New() huma.API {
	config := huma.DefaultConfig("formats", "1")
	config.Formats = huma.DefaultFormats          // want "huma.DefaultFormats and huma.DefaultJSONFormat encode with encoding/json \(v1\)"
	return humago.New(http.NewServeMux(), config) // want "encoding/json \(v1\) formats"
}
