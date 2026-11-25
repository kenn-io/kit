package embedclient

import (
	"errors"
	"fmt"
	"strings"

	json "encoding/json/v2"
)

type vertexInstance struct {
	Content  string `json:"content"`
	TaskType string `json:"task_type"`
}

func (c *Client) vertexRequestBody(texts []string) ([]byte, error) {
	if strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		body, err := json.Marshal(map[string]any{
			"content":            map[string]any{"parts": []any{map[string]any{"text": texts[0]}}},
			"embedContentConfig": map[string]any{"outputDimensionality": c.model.Dimensions},
		})
		if err != nil {
			return nil, fmt.Errorf("embed request encoding failed: %w", err)
		}
		return body, nil
	}
	instances := make([]vertexInstance, len(texts))
	for i, text := range texts {
		instances[i] = vertexInstance{Content: text, TaskType: "RETRIEVAL_DOCUMENT"}
	}
	var request struct {
		Instances  []vertexInstance `json:"instances"`
		Parameters struct {
			OutputDimensionality int  `json:"outputDimensionality"`
			AutoTruncate         bool `json:"autoTruncate"`
		} `json:"parameters"`
	}
	request.Instances = instances
	request.Parameters.OutputDimensionality = c.model.Dimensions
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
			Truncated bool `json:"truncated"`
		}
		if err := json.Unmarshal(payload, &response); err != nil {
			return nil, nil, errors.New("embed response is invalid")
		}
		if response.Truncated {
			return nil, nil, errors.New("embed response was truncated")
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
