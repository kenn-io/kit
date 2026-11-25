package embedclient

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	json "encoding/json/v2"

	"go.kenn.io/kit/embedconfig"
)

type vertexInstance struct {
	Content  string `json:"content"`
	TaskType string `json:"task_type"`
}

func (c *Client) vertexRequestBody(inputs []encoded) ([]byte, error) {
	if strings.HasPrefix(c.model.Name, "gemini-embedding-2") {
		body, err := json.Marshal(map[string]any{
			"content":            map[string]any{"parts": []any{map[string]any{"text": inputs[0].text}}},
			"embedContentConfig": map[string]any{"outputDimensionality": c.model.Dimensions},
		})
		if err != nil {
			return nil, fmt.Errorf("embed request encoding failed: %w", err)
		}
		return body, nil
	}
	instances := make([]vertexInstance, len(inputs))
	for i, input := range inputs {
		taskType := "RETRIEVAL_DOCUMENT"
		if input.role == embedconfig.RoleQuery {
			taskType = "RETRIEVAL_QUERY"
		}
		instances[i] = vertexInstance{Content: input.text, TaskType: taskType}
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
			return nil, nil, &APIError{StatusCode: http.StatusOK, Reason: ReasonInputTooLong}
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
