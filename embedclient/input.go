package embedclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

// Part contains text or media bytes and their media type.
type Part struct {
	Text      string `json:"text,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      []byte `json:"data,omitempty"`
}

// Input is one query or document composed of parts.
type Input struct {
	Role  embedconfig.Role `json:"role"`
	Parts []Part           `json:"parts"`
}

// ErrInvalidDimensions reports a size the backend cannot serve.
var ErrInvalidDimensions = errors.New("embed dimensions are invalid")

// Backend encodes inputs as given, without adding role affixes.
type Backend interface {
	Encode(context.Context, int, []Input) ([][]float32, error)
}

func (in Input) validate() error {
	if in.Role != embedconfig.RoleQuery && in.Role != embedconfig.RoleDocument {
		return errors.New("embed role must be document or query")
	}
	if len(in.Parts) == 0 {
		return errors.New("embed input needs parts")
	}
	for i, p := range in.Parts {
		kind := embedmodel.KindText
		if p.MediaType != "" {
			kind = embedmodel.KindFile
			if strings.HasPrefix(p.MediaType, "image/") {
				kind = embedmodel.KindImage
			}
		}
		if (p.MediaType == "" && len(p.Data) != 0) || (p.MediaType != "" && len(p.Data) == 0) {
			return fmt.Errorf("part %d: media needs a type and data", i)
		}
		if err := (embedmodel.Part{Kind: kind, Text: p.Text, MediaType: p.MediaType}).Validate(); err != nil {
			return fmt.Errorf("part %d: %w", i, err)
		}
	}
	return nil
}

// Validate checks the input's role and parts.
func (in Input) Validate() error { return in.validate() }

func hasMedia(parts []Part) bool {
	for _, p := range parts {
		if p.MediaType != "" {
			return true
		}
	}
	return false
}

// EmbedInputs applies role affixes to text-only inputs and encodes them.
func (c *Client) EmbedInputs(ctx context.Context, inputs []Input) ([][]float32, error) {
	return c.encodeInputs(ctx, 0, inputs, true)
}

// Encode encodes inputs as given. Dimensions must match the configured size.
func (c *Client) Encode(ctx context.Context, dims int, inputs []Input) ([][]float32, error) {
	return c.encodeInputs(ctx, dims, inputs, false)
}

func (c *Client) encodeInputs(ctx context.Context, dims int, inputs []Input, format bool) ([][]float32, error) {
	if dims != 0 && dims != c.model.Dimensions {
		return nil, fmt.Errorf("%w: differs from client configuration", ErrInvalidDimensions)
	}
	prepared := make([]encoded, len(inputs))
	for i, in := range inputs {
		if err := in.validate(); err != nil {
			return nil, fmt.Errorf("embed input %d: %w", i, err)
		}
		media := hasMedia(in.Parts)
		if media && c.api != embedconfig.APIKit && (c.api != embedconfig.APIVertex || !strings.HasPrefix(c.model.Name, "gemini-embedding-2")) {
			return nil, fmt.Errorf("embed input %d: %w", i, embedmodel.ErrUnsupportedContent)
		}
		parts := append([]Part(nil), in.Parts...)
		var texts []string
		for _, p := range parts {
			if p.MediaType == "" {
				texts = append(texts, p.Text)
			}
		}
		text := strings.Join(texts, "\n")
		if !media {
			if embedmodel.BlankText(text) {
				return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
			}
			if format {
				var err error
				text, err = embedmodel.Format(in.Role, text, c.roles)
				if err != nil {
					return nil, err
				}
			}
			parts = []Part{{Text: text}}
		}
		prepared[i] = encoded{index: i, role: in.Role, text: text, parts: parts}
	}
	return c.embedPrepared(ctx, prepared)
}
