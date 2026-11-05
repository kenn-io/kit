package pack

import (
	"bytes"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/stretchr/testify/require"
)

func TestPackRoundTripProperty(t *testing.T) {
	key := testKey(21)
	crypter, err := NewCrypter(key)
	require.NoError(t, err)

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

	config := &quick.Config{
		MaxCount: 40,
		Values: func(values []reflect.Value, rng *rand.Rand) {
			// Keep the previous generator's size bound and blob shapes.
			blobs := make([][]byte, rng.Intn(100))
			for i := range blobs {
				switch rng.Intn(4) {
				case 0:
					blobs[i] = []byte{}
				case 1:
					blobs[i] = []byte{byte(rng.Intn(256))}
				case 2:
					blobs[i] = bytes.Repeat([]byte("pattern!"), 1+rng.Intn(2048))
				case 3:
					blobs[i] = make([]byte, rng.Intn(100))
					for j := range blobs[i] {
						blobs[i][j] = byte(rng.Intn(256))
					}
				}
			}
			values[0] = reflect.ValueOf(blobs)
		},
	}
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
			require.NoError(t, quick.Check(property, config))
		})
	}
}
