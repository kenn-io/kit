// Package humaconfig mirrors a module-local helper that installs JSON v2.
package humaconfig

import (
	jsonv2 "encoding/json/v2"
	"io"
	"maps"

	"github.com/danielgtaylor/huma/v2"
)

func Configure(cfg huma.Config) huma.Config {
	format := huma.Format{
		Marshal: func(w io.Writer, value any) error {
			return jsonv2.MarshalWrite(w, value)
		},
		Unmarshal: func(data []byte, value any) error {
			return jsonv2.Unmarshal(data, value)
		},
	}
	formats := maps.Clone(cfg.Formats)
	formats["application/json"] = format
	formats["json"] = format
	cfg.Formats = formats
	return cfg
}

func New(title string) huma.Config {
	return Configure(huma.DefaultConfig(title, "1"))
}
