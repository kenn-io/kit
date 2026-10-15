package gcs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInventoryPreservesForeignEntries(t *testing.T) {
	f, s := newFixture(t, "")
	for _, key := range []string{"a", "foreign/\x01+entry", "foreign/../entry", "z"} {
		f.objects["namespace/"+key] = fixtureObject{Data: []byte(key), Generation: "1"}
	}
	keys := []string{}
	token := ""
	for {
		page, err := s.ListPage(t.Context(), "", token, 1)
		require.NoError(t, err)
		for _, obj := range page.Objects {
			keys = append(keys, obj.Key)
			assert.Equal(t, "1", obj.Revision)
			assert.Equal(t, "UTC", obj.LastModified.Location().String())
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	assert.Equal(t, []string{"a", "foreign/\x01+entry", "foreign/../entry", "z"}, keys)
	assert.Len(t, f.objects, 4)
}
