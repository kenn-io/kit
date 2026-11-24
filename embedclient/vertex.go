package embedclient

import (
	"errors"
	"fmt"
	"strings"

	json "encoding/json/v2"
)

func (c *Client) vertexRequestBody(texts []string) ([]byte, error) {
	var request any
	if strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		request = map[string]any{
			"content":            map[string]any{"parts": []any{map[string]any{"text": texts[0]}}},
			"embedContentConfig": map[string]any{"outputDimensionality": c.model.Dimensions},
		}
	} else {
		request = map[string]any{
			"instances":  []any{map[string]any{"content": texts[0], "task_type": "RETRIEVAL_DOCUMENT"}},
			"parameters": map[string]any{"outputDimensionality": c.model.Dimensions},
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("embed request encoding failed: %w", err)
	}
	return body, nil
}

func (c *Client) classifyVertex(payload []byte, count int) ([][]float32, []error, error) {
	if strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		var response struct {
			Embedding struct {
				Values wireEmbedding `json:"values"`
			} `json:"embedding"`
		}
		if err := json.Unmarshal(payload, &response); err != nil {
			return nil, nil, errors.New("embed response is invalid")
		}
		out, problems := c.decodeVectors([]wireEmbedding{response.Embedding.Values})
		return out, problems, nil
	}
	var response struct {
		Predictions []struct {
			Embeddings struct {
				Values wireEmbedding `json:"values"`
			} `json:"embeddings"`
		} `json:"predictions"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, nil, errors.New("embed response is invalid")
	}
	if len(response.Predictions) != count {
		return nil, nil, fmt.Errorf("embed response contained %d vectors for %d inputs", len(response.Predictions), count)
	}
	raw := make([]wireEmbedding, count)
	for i, item := range response.Predictions {
		raw[i] = item.Embeddings.Values
	}
	out, problems := c.decodeVectors(raw)
	return out, problems, nil
}
