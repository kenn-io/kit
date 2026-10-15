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

func TestListPagePreservesEncodedNames(t *testing.T) {
	f, s := newFixture(t, "")
	f.objects["namespace/foreign/a %/value"] = fixtureObject{Data: []byte("first"), Generation: "1"}
	f.objects["namespace/foreign/b/last"] = fixtureObject{Data: []byte("second"), Generation: "2"}
	first, err := s.ListPage(t.Context(), "foreign/", "", 1)
	require.NoError(t, err)
	require.Len(t, first.Objects, 1)
	assert.Equal(t, "foreign/a %/value", first.Objects[0].Key)
	require.NotEmpty(t, first.NextToken)
	second, err := s.ListPage(t.Context(), "foreign/", first.NextToken, 1)
	require.NoError(t, err)
	require.Len(t, second.Objects, 1)
	assert.Equal(t, "foreign/b/last", second.Objects[0].Key)
	assert.Empty(t, second.NextToken)
	assert.Len(t, f.objects, 2)
}
