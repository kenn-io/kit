package pack

import (
	"bytes"
	"flag"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

func TestPackRoundTripProperty(t *testing.T) {
	key := testKey(21)
	crypter, err := NewCrypter(key)
	require.NoError(t, err)

	// Rapid configures the case count globally; restore it after this test.
	previousChecks := flag.Lookup("rapid.checks").Value.String()
	checks := "40"
	if testing.Short() {
		checks = "200" // Rapid divides the case count by five in short mode.
	}
	require.NoError(t, flag.Set("rapid.checks", checks))
	t.Cleanup(func() { require.NoError(t, flag.Set("rapid.checks", previousChecks)) })

	roundTrip := func(c *Crypter) func(blobs [][]byte) bool {
		return func(blobs [][]byte) bool {
			dir := t.TempDir()
			w, err := NewWriter(dir, WriterOptions{Crypter: c})
			if err != nil {
				return false
			}
			wrote := make([]Entry, 0, len(blobs))
			for _, b := range blobs {
				e, err := w.Append(b)
				if err != nil {
					return false
				}
				wrote = append(wrote, e)
			}
			path := filepath.Join(dir, w.ID()+".mvpack")
			sealedEntries, err := w.Seal(path)
			if err != nil || len(sealedEntries) != len(wrote) {
				return false
			}
			r, err := OpenReader(path, c)
			if err != nil {
				return false
			}
			defer func() { _ = r.Close() }()
			got := r.Entries()
			if len(got) != len(blobs) {
				return false
			}
			for i, e := range got {
				if e != wrote[i] {
					return false
				}
				raw, err := r.ReadBlob(e)
				if err != nil || !bytes.Equal(raw, blobs[i]) {
					return false
				}
			}
			return true
		}
	}

	// Keep the original blob shapes and exclusive size bound of 100.
	blob := rapid.OneOf(
		rapid.Just([]byte{}),
		rapid.SliceOfN(rapid.Byte(), 1, 1),
		rapid.Map(rapid.IntRange(1, 2048), func(n int) []byte {
			return bytes.Repeat([]byte("pattern!"), n)
		}),
		rapid.SliceOfN(rapid.Byte(), 0, 99),
	)
	blobSets := rapid.SliceOfN(blob, 0, 99)
	for _, tc := range []struct {
		name    string
		crypter *Crypter
	}{
		{name: "plain"},
		{name: "encrypted", crypter: crypter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			property := roundTrip(tc.crypter)
			require.True(t, property(nil), "empty pack must round-trip")
			rapid.Check(t, func(t *rapid.T) {
				require.True(t, property(blobSets.Draw(t, "blobs")))
			})
		})
	}
}
