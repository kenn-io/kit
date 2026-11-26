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

// Input is one query or document containing text or media.
type Input struct {
	Role      embedconfig.Role `json:"role"`
	Text      string           `json:"text,omitempty"`
	MediaType string           `json:"media_type,omitempty"`
	Data      []byte           `json:"data,omitempty"`
}

// ErrInvalidDimensions reports a size the backend cannot serve.
var ErrInvalidDimensions = errors.New("embed dimensions are invalid")

// Backend encodes inputs as given, without adding role affixes.
type Backend interface {
	Encode(context.Context, int, []Input) ([][]float32, error)
}

// Validate checks the input's role and payload.
func (in Input) Validate() error {
	if in.Role != embedconfig.RoleQuery && in.Role != embedconfig.RoleDocument {
		return errors.New("embed role must be document or query")
	}
	if in.Text != "" {
		if in.MediaType != "" || len(in.Data) != 0 || embedmodel.BlankText(in.Text) {
			return errors.New("embed input needs text or media")
		}
		return nil
	}
	if strings.TrimSpace(in.MediaType) == "" || len(in.Data) == 0 {
		return errors.New("embed input needs text or media")
	}
	return nil
}

// EmbedInputs applies role affixes to text inputs and encodes them.
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
		if err := in.Validate(); err != nil {
			return nil, fmt.Errorf("embed input %d: %w", i, err)
		}
		if in.MediaType != "" && c.api != embedconfig.APIKit && (c.api != embedconfig.APIVertex || !strings.HasPrefix(c.model.Name, "gemini-embedding-2")) {
			return nil, fmt.Errorf("embed input %d: %w", i, embedmodel.ErrUnsupportedContent)
		}
		if format && in.MediaType == "" {
			var err error
			in.Text, err = c.formatText(in.Role, in.Text)
			if err != nil {
				return nil, err
			}
			if embedmodel.BlankText(in.Text) {
				return nil, fmt.Errorf("embed input %d: %w", i, vector.ErrEmptyEmbeddingInput)
			}
		}
		prepared[i] = encoded{index: i, role: in.Role, text: in.Text, input: in}
	}
	return c.embedPrepared(ctx, prepared)
}

func (c *Client) formatText(role embedconfig.Role, text string) (string, error) {
	if c.api == embedconfig.APIKit && strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		if err := validateGeminiEmbedding2Roles(c.roles); err != nil {
			return "", err
		}
	}
	return embedmodel.Format(role, text, c.roles)
}
