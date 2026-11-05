package embedtest

import (
	"encoding/json/v2"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
)

// AssertFaces exercises every annotated astronaut and the two-person photograph.
func AssertFaces(t *testing.T, client *embedclient.Client, dir, annotations string) {
	t.Helper()
	data, err := os.ReadFile(annotations)
	require.NoError(t, err)
	var samples []struct {
		File      string              `json:"file"`
		Person    string              `json:"person"`
		Box       embedclient.FaceBox `json:"box"`
		Unmatched bool                `json:"unmatched"`
	}
	require.NoError(t, json.Unmarshal(data, &samples))
	type embedded struct {
		person, file string
		vector       []float32
	}
	var vectors []embedded
	for start := 0; start < len(samples); {
		end := start + 1
		for end < len(samples) && samples[end].File == samples[start].File {
			end++
		}
		photo, err := os.ReadFile(filepath.Join(dir, "faces", samples[start].File))
		require.NoError(t, err)
		boxes := make([]embedclient.FaceBox, end-start)
		for i := start; i < end; i++ {
			boxes[i-start] = samples[i].Box
		}
		result, err := client.EmbedFaces(t.Context(), photo, boxes)
		require.NoError(t, err, samples[start].File)
		require.Len(t, result.Data, len(boxes))
		for i, item := range result.Data {
			sample := samples[start+i]
			if sample.Unmatched {
				require.Nil(t, item.Embedding, sample.File)
				t.Logf("no face detected: %s", sample.File)
				continue
			}
			require.Len(t, item.Embedding, 512, sample.File+" "+sample.Person)
			var norm float64
			for _, v := range item.Embedding {
				norm += float64(v) * float64(v)
			}
			assert.InDelta(t, 1, norm, 1e-5)
			vectors = append(vectors, embedded{sample.Person, sample.File, item.Embedding})
		}
		start = end
	}
	maxSame, minDifferent := 0.0, math.Inf(1)
	for i, a := range vectors {
		for _, b := range vectors[:i] {
			var sum float64
			for k, v := range a.vector {
				delta := float64(v) - float64(b.vector[k])
				sum += delta * delta
			}
			distance := math.Sqrt(sum)
			t.Logf("%s (%s) / %s (%s): %.6f", a.file, a.person, b.file, b.person, distance)
			if a.person == b.person {
				maxSame = max(maxSame, distance)
			} else {
				minDifferent = min(minDifferent, distance)
			}
		}
	}
	require.Less(t, maxSame, minDifferent, "the corpus must separate by identity")
	threshold := 1.23
	if maxSame >= threshold || minDifferent <= threshold {
		threshold = (maxSame + minDifferent) / 2
	}
	t.Logf("maximum same-person %.6f; minimum different-person %.6f; separating distance %.6f", maxSame, minDifferent, threshold)
}
