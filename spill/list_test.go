package spill_test

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/spill"
)

func collect(l *spill.List) ([]string, error) {
	var records []string
	for record, err := range l.All() {
		if err != nil {
			return records, err
		}
		records = append(records, record)
	}
	return records, nil
}

// onlyFile returns the path of the single entry in dir.
func onlyFile(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	return filepath.Join(dir, entries[0].Name())
}

func TestListUnderLimitStaysInMemory(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	l := spill.New(dir, "spill-*", 10)
	t.Cleanup(func() { require.NoError(l.Close()) })

	// The payload reaches the limit exactly, which still fits in memory.
	for _, s := range []string{"abc", "", "defg", "hij"} {
		require.NoError(l.Append(s))
	}

	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"abc", "", "defg", "hij"}, records)
	assert.Equal(t, 4, l.Len())
	assert.Equal(t, int64(10), l.Size())
	assert.False(t, l.Spilled())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(t, entries)
}

func TestListOverLimitMovesToPrivateFile(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	l := spill.New(dir, "spill-*.tmp", 4)
	t.Cleanup(func() { require.NoError(l.Close()) })

	require.NoError(l.Append("ab"))
	require.NoError(l.Append("cd"))
	require.False(l.Spilled())
	require.NoError(l.Append("e"))
	require.True(l.Spilled())

	path := onlyFile(t, dir)
	assert.True(t, strings.HasPrefix(filepath.Base(path), "spill-"))
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(t, fs.FileMode(0o600), info.Mode().Perm())
	}

	require.NoError(l.Append("fgh"))
	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"ab", "cd", "e", "fgh"}, records)

	// Appending after a read continues the same list.
	require.NoError(l.Append("ij"))
	records, err = collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"ab", "cd", "e", "fgh", "ij"}, records)
	assert.Equal(t, 5, l.Len())
	assert.Equal(t, int64(10), l.Size())
}

func TestListIterationYieldsRecordsAppendedBeforeItStarted(t *testing.T) {
	require := require.New(t)
	l := spill.New(t.TempDir(), "spill-*", 3)
	t.Cleanup(func() { require.NoError(l.Close()) })
	require.NoError(l.Append("a"))
	require.NoError(l.Append("b"))

	// The first pass starts in memory and moves the list to disk midway; the
	// second starts on disk and appends to the file.
	for range 2 {
		var seen []string
		for record, err := range l.All() {
			require.NoError(err)
			seen = append(seen, record)
			require.NoError(l.Append(record + record))
		}
		assert.Len(t, seen, l.Len()/2)
	}
	assert.True(t, l.Spilled())

	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"a", "b", "aa", "bb", "aa", "bb", "aaaa", "bbbb"}, records)
}

func TestListRoundTripsArbitraryBytes(t *testing.T) {
	var everyByte strings.Builder
	for b := range 256 {
		everyByte.WriteByte(byte(b))
	}
	values := []string{"", "a\x00b", "\x00", "line\n", "\r\n", "", everyByte.String(), strings.Repeat("x", 200_000), ""}

	for name, limit := range map[string]int64{"memory": 1 << 20, "file": -1} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			l := spill.New(t.TempDir(), "spill-*", limit)
			t.Cleanup(func() { require.NoError(l.Close()) })
			for _, v := range values {
				require.NoError(l.Append(v))
			}
			require.Equal(name == "file", l.Spilled())

			records, err := collect(l)
			require.NoError(err)
			assert.Equal(t, values, records)
		})
	}
}

func TestListCloseRemovesFile(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	l := spill.New(dir, "spill-*", 0)
	require.NoError(l.Append("record"))
	onlyFile(t, dir)

	require.NoError(l.Close())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	assert.Empty(t, entries)

	require.NoError(l.Close())
	require.ErrorIs(l.Append("more"), fs.ErrClosed)
	_, err = collect(l)
	require.ErrorIs(err, fs.ErrClosed)
}

func TestListAppendReportsFileCreationFailure(t *testing.T) {
	require := require.New(t)
	l := spill.New(filepath.Join(t.TempDir(), "missing"), "spill-*", 1)
	t.Cleanup(func() { require.NoError(l.Close()) })
	require.NoError(l.Append("a"))

	require.ErrorIs(l.Append("b"), fs.ErrNotExist)

	// The failed append leaves the list as it was.
	assert.False(t, l.Spilled())
	assert.Equal(t, 1, l.Len())
	records, err := collect(l)
	require.NoError(err)
	assert.Equal(t, []string{"a"}, records)
}

func TestListReadReportsDamagedFile(t *testing.T) {
	// The file holds the length-prefixed records 02 'a' 'b' 02 'c' 'd' 02 'e' 'f'.
	tests := map[string]struct {
		damage func([]byte) []byte
		want   error
	}{
		"truncated inside a record": {
			damage: func(b []byte) []byte { return b[:len(b)-1] },
			want:   io.ErrUnexpectedEOF,
		},
		"truncated before a length": {
			damage: func(b []byte) []byte { return b[:6] },
			want:   io.ErrUnexpectedEOF,
		},
		"length past the payload": {
			damage: func(b []byte) []byte { b[3] = 0x05; return b },
		},
		"length too short": {
			damage: func(b []byte) []byte { b[0] = 0x01; return b },
		},
		"length overflow": {
			damage: func(b []byte) []byte {
				return append([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, b[1:]...)
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			l := spill.New(dir, "spill-*", 0)
			t.Cleanup(func() { require.NoError(l.Close()) })
			for _, s := range []string{"ab", "cd", "ef"} {
				require.NoError(l.Append(s))
			}
			// Reading flushes the records to the file.
			_, err := collect(l)
			require.NoError(err)

			path := onlyFile(t, dir)
			data, err := os.ReadFile(path)
			require.NoError(err)
			require.Equal([]byte("\x02ab\x02cd\x02ef"), data)
			require.NoError(os.WriteFile(path, tt.damage(data), 0o600))

			records, err := collect(l)
			require.Error(err)
			if tt.want != nil {
				require.ErrorIs(err, tt.want)
			}
			assert.Less(t, len(records), 3)
		})
	}
}
