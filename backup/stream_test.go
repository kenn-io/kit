package backup

import (
	"context"
	"io"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestRepoOpenBlobStreamsVerifiedContent(t *testing.T) {
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	content := []byte("repository streaming content")
	id, _, err := appender.Add(content)
	require.NoError(t, err)
	_, _, err = appender.Finish()
	require.NoError(t, err)

	stream, err := r.OpenBlob(context.Background(), known, id, nil, testPackExt)
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), stream.Size())
	prefix := make([]byte, 4)
	_, err = io.ReadFull(stream, prefix)
	require.NoError(t, err)
	assert.False(t, stream.Verified())
	rest, err := io.ReadAll(stream)
	require.NoError(t, err)
	assert.Equal(t, content, append(prefix, rest...))
	assert.True(t, stream.Verified())
	require.NoError(t, stream.Close())
}

func TestRepoOpenBlobRejectsIndexMismatchBeforeStreaming(t *testing.T) {
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	id, _, err := appender.Add([]byte("indexed content"))
	require.NoError(t, err)
	_, _, err = appender.Finish()
	require.NoError(t, err)
	for _, tc := range []struct {
		name  string
		forge func(*IndexEntry)
	}{
		{name: "offset", forge: func(entry *IndexEntry) { entry.Offset++ }},
		{name: "stored length", forge: func(entry *IndexEntry) { entry.StoredLen++ }},
		{name: "flags", forge: func(entry *IndexEntry) { entry.Flags ^= pack.BlobCompressed }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forgedKnown := maps.Clone(known)
			forged := forgedKnown[id]
			tc.forge(&forged)
			forgedKnown[id] = forged

			stream, err := r.OpenBlob(context.Background(), forgedKnown, id, nil, testPackExt)
			require.ErrorContains(t, err, "index metadata disagrees")
			assert.Nil(t, stream)
		})
	}
}

func TestRepoOpenBlobEarlyCloseIsUnverified(t *testing.T) {
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	id, _, err := appender.Add([]byte("early close"))
	require.NoError(t, err)
	_, _, err = appender.Finish()
	require.NoError(t, err)

	stream, err := r.OpenBlob(context.Background(), known, id, nil, testPackExt)
	require.NoError(t, err)
	_, err = stream.Read(make([]byte, 1))
	require.NoError(t, err)
	require.ErrorIs(t, stream.Close(), pack.ErrVerificationIncomplete)
	require.ErrorIs(t, stream.Close(), pack.ErrVerificationIncomplete)
}
