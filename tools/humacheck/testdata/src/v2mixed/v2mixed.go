package v2mixed

import (
	"encoding/json/v2"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"thirdparty/codec"
)

var v2Format = huma.Format{
	Marshal:   func(w io.Writer, v any) error { return json.MarshalWrite(w, v) },
	Unmarshal: func(data []byte, v any) error { return json.Unmarshal(data, v) },
}

// mixedConfig is v2 on one path and v1 on the other; every return must agree.
func mixedConfig(strict bool) huma.Config {
	if strict {
		cfg := huma.DefaultConfig("mixed", "1")
		cfg.Formats = map[string]huma.Format{"application/json": v2Format}
		return cfg
	}
	return huma.DefaultConfig("mixed", "1")
}

func NewMixed() huma.API {
	return humago.New(http.NewServeMux(), mixedConfig(true)) // want "encoding/json \(v1\) formats"
}

// NewLate reconfigures the config only after the API is built.
func NewLate() huma.API {
	cfg := huma.DefaultConfig("late", "1")
	api := humago.New(http.NewServeMux(), cfg) // want "encoding/json \(v1\) formats"
	cfg.Formats = map[string]huma.Format{"application/json": v2Format}
	return api
}

// installOverride would make the defaults v2, but nothing calls it, so it
// does not count as a process-wide override.
func installOverride() {
	huma.DefaultJSONFormat = v2Format
	huma.DefaultFormats["application/json"] = huma.DefaultJSONFormat
}

func NewUncalledOverride() huma.API {
	return humago.New(http.NewServeMux(), huma.DefaultConfig("uncalled", "1")) // want "encoding/json \(v1\) formats"
}

// settle installs v2 on its parameter before constructing, so callers may
// pass anything.
func settle(mux *http.ServeMux, cfg huma.Config) huma.API {
	cfg.Formats = map[string]huma.Format{"application/json": v2Format}
	return humago.New(mux, cfg)
}

func NewSettled() huma.API {
	return settle(http.NewServeMux(), huma.DefaultConfig("settled", "1"))
}

// NewExternal uses a codec the checker cannot inspect: unverifiable, not v1.
func NewExternal() huma.API {
	cfg := huma.DefaultConfig("external", "1")
	cfg.Formats = map[string]huma.Format{"application/json": {Marshal: codec.Marshal, Unmarshal: codec.Unmarshal}}
	return humago.New(http.NewServeMux(), cfg) // want "cannot verify"
}

// NewOverwritten installs v2 and then replaces it with the v1 defaults.
func NewOverwritten() huma.API {
	cfg := huma.DefaultConfig("overwritten", "1")
	cfg.Formats = map[string]huma.Format{"application/json": v2Format}
	cfg = huma.DefaultConfig("overwritten", "2")
	return humago.New(http.NewServeMux(), cfg) // want "encoding/json \(v1\) formats"
}
