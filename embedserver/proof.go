package embedserver

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/daemon"
)

// LoadProof reads the opaque endpoint proof capability from the server data dir.
// The caller passes it to daemon discovery or Stop before contacting a record.
func LoadProof(dir string) (*daemon.Proof, error) {
	key, err := os.ReadFile(filepath.Join(dir, "proof.key"))
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid embeddings proof key")
	}
	return daemon.NewProof(key)
}

func ensureProof(dir string) (*daemon.Proof, error) {
	proof, err := LoadProof(dir)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		return proof, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := atomicfile.WriteFile(filepath.Join(dir, "proof.key"), key, atomicfile.WithPrivate()); err != nil {
		return nil, err
	}
	return daemon.NewProof(key)
}
