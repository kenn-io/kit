package backup

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeObjectRecipeStopsAtInvalidChunk(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const valid = `{"blob":"` + hash + `","bytes":1}`
	for _, invalid := range []string{
		`null`,
		`{"blob":"` + hash + `","bytes":0}`,
		`{"blob":"invalid","bytes":1}`,
		`{"blob":"` + hash + `","bytes":1,"extra":true}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			raw := []byte(`{"version":1,"blob":"` + hash + `","bytes":258,"chunks":[` +
				valid + `,` + invalid + strings.Repeat(`,`+valid, 256) + `]}`)
			recipe, err := decodeObjectRecipe(raw)
			require.Error(t, err)
			assert.Len(t, recipe.Chunks, 1, "stop before retaining the invalid chunk or its suffix")
		})
	}
}

func TestDecodeObjectRecipeChunkLimit(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const chunk = `{"blob":"` + hash + `","bytes":1}`
	for _, count := range []int{maxObjectChunks, maxObjectChunks + 1} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			var raw bytes.Buffer
			raw.Grow(count*(len(chunk)+1) + 160)
			fmt.Fprintf(&raw, `{"version":1,"blob":"%s","bytes":%d,"chunks":[`, hash, count)
			for i := range count {
				if i > 0 {
					raw.WriteByte(',')
				}
				raw.WriteString(chunk)
			}
			raw.WriteString(`]}`)
			require.Less(t, raw.Len(), maxRecipeBytes, "the byte bound permits this input")
			recipe, err := decodeObjectRecipe(raw.Bytes())
			if count == maxObjectChunks {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			assert.Len(t, recipe.Chunks, maxObjectChunks, "never retain more than the chunk limit")
		})
	}
}

func TestDecodeObjectRecipeTrailingData(t *testing.T) {
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const chunk = `{"blob":"` + hash + `","bytes":1}`
	const recipe = `{"version":1,"blob":"` + hash + `","bytes":2,"chunks":[` + chunk + `,` + chunk + `]}`
	for _, suffix := range []string{"", " \n\t", " [null]", " null", " }"} {
		t.Run(fmt.Sprintf("%q", suffix), func(t *testing.T) {
			_, err := decodeObjectRecipe([]byte(recipe + suffix))
			if strings.TrimSpace(suffix) == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "trailing object recipe data")
			}
		})
	}
}
