package v1codec

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

func New() huma.API {
	config := huma.DefaultConfig("codec", "1")
	format := huma.Format{
		Marshal: func(w io.Writer, value any) error {
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			return enc.Encode(value)
		},
		Unmarshal: json.Unmarshal,
	}
	config.Formats = map[string]huma.Format{"application/json": format, "json": format}
	return humago.New(http.NewServeMux(), config) // want "encoding/json \(v1\) formats"
}
