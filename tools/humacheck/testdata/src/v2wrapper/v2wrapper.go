package v2wrapper

import (
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// marshalAPIJSON uses v2 with v1-compatible options; the options call does
// not make it a v1 codec.
func marshalAPIJSON(w io.Writer, value any) error {
	if err := jsonv2.MarshalWrite(w, value, jsonv1.DefaultOptionsV1()); err != nil {
		return fmt.Errorf("marshal API JSON: %w", err)
	}
	return nil
}

func unmarshalAPIJSON(data []byte, value any) error {
	return decode(data, value)
}

func decode(data []byte, value any) error {
	return jsonv2.Unmarshal(data, value, jsonv1.DefaultOptionsV1())
}

func New() huma.API {
	config := huma.DefaultConfig("wrapper", "1")
	jsonFormat := huma.Format{
		Marshal:   marshalAPIJSON,
		Unmarshal: unmarshalAPIJSON,
	}
	config.Formats = map[string]huma.Format{
		"application/json": jsonFormat,
		"json":             jsonFormat,
	}
	return humago.New(http.NewServeMux(), config)
}
