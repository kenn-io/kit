package backup

import (
	"context"
	"io"
	"maps"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestRepoOpenBlobStreamsVerifiedContent(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	content := []byte("repository streaming content")
	id, _, err := appender.Add(content)
	require.NoError(err)
	_, _, err = appender.Finish()
	require.NoError(err)

	stream, err := r.OpenBlob(context.Background(), known, id, nil, testPackExt)
	require.NoError(err)
	assert.Equal(int64(len(content)), stream.Size())
	prefix := make([]byte, 4)
	_, err = io.ReadFull(stream, prefix)
	require.NoError(err)
	assert.False(stream.Verified())
	rest, err := io.ReadAll(stream)
	require.NoError(err)
	assert.Equal(content, append(prefix, rest...))
	assert.True(stream.Verified())
	require.NoError(stream.Close())
}

func TestRepoOpenBlobRejectsIndexMismatchBeforeStreaming(t *testing.T) {
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	id, _, err := appender.Add([]byte("indexed content"))
	Require.NoError(t, err)
	_, _, err = appender.Finish()
	Require.NoError(t, err)
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
			Require.ErrorContains(t, err, "index metadata disagrees")
			Assert.Nil(t, stream)
		})
	}
}

func TestRepoOpenBlobEarlyCloseIsUnverified(t *testing.T) {
	require := Require.New(t)
	r := initTestRepo(t)
	known := map[pack.BlobID]IndexEntry{}
	appender := NewPackAppender(r, known, pack.DefaultZstdLevel, nil, testPackExt)
	id, _, err := appender.Add([]byte("early close"))
	require.NoError(err)
	_, _, err = appender.Finish()
	require.NoError(err)

	stream, err := r.OpenBlob(context.Background(), known, id, nil, testPackExt)
	require.NoError(err)
	_, err = stream.Read(make([]byte, 1))
	require.NoError(err)
	require.ErrorIs(stream.Close(), pack.ErrVerificationIncomplete)
	require.ErrorIs(stream.Close(), pack.ErrVerificationIncomplete)
}
