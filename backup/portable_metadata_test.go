package backup_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/kit/pack"
)

type portableRecord struct {
	Notes []string       `json:"notes"`
	Files []portableFile `json:"files"`
}

type portableFile struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Path string `json:"path"`
}

type portableStats struct {
	Notes int64 `json:"notes"`
	Files int64 `json:"files"`
}

type portableApp struct{}

func (portableApp) FrozenView(*backup.FrozenSession) backup.FrozenView { panic("not used") }
func (portableApp) DBFileName() string                                 { return "portable.db" }
func (portableApp) ContentDirName() string                             { return "content" }
func (portableApp) PackFileExtension() string                          { return ".kpack" }
func (portableApp) Version() string                                    { return "portable-test" }
func (portableApp) ExcludedPaths() []string                            { return nil }
func (portableApp) CheckManifest(m *backup.Manifest) []string {
	var stats portableStats
	if err := json.Unmarshal(m.Stats, &stats); err != nil {
		return []string{err.Error()}
	}
	if stats.Files != m.Attachments.Blobs {
		return []string{"file count differs from attachments"}
	}
	return nil
}
func (portableApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT hash, path FROM files ORDER BY hash`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	paths := map[string][]string{}
	for rows.Next() {
		var hash, path string
		if err := rows.Scan(&hash, &path); err != nil {
			return nil, err
		}
		paths[hash] = append(paths[hash], path)
	}
	return paths, rows.Err()
}
func (portableApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	var stats portableStats
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes`).Scan(&stats.Notes); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&stats.Files); err != nil {
		return nil, err
	}
	return json.Marshal(stats)
}

type portableSource struct {
	raw            []byte
	info           *backup.ContentInfo
	stats          json.RawMessage
	opened         bool
	closed         bool
	closes         int
	closeErr       error
	metadataReader io.ReadCloser
	metadataErr    error
}

func (s *portableSource) Format() string { return "test-json-v1" }
func (s *portableSource) OpenSnapshot(context.Context) (backup.MetadataSnapshot, error) {
	s.opened = true
	return s, nil
}
func (s *portableSource) OpenMetadata(context.Context) (io.ReadCloser, int64, error) {
	reader := s.metadataReader
	if reader == nil {
		reader = io.NopCloser(bytes.NewReader(s.raw))
	}
	return reader, int64(len(s.raw)), s.metadataErr
}
func (s *portableSource) ContentInfo(context.Context) (*backup.ContentInfo, error) {
	return s.info, nil
}
func (s *portableSource) Stats(context.Context) (json.RawMessage, error) { return s.stats, nil }
func (s *portableSource) Close() error {
	s.closed = true
	s.closes++
	return s.closeErr
}

type closeCounter struct{ closes int }

func (*closeCounter) Read([]byte) (int, error) { return 0, io.EOF }
func (r *closeCounter) Close() error           { r.closes++; return nil }

type portableRestorer struct{}

func (portableRestorer) RestoreMetadata(
	ctx context.Context, format string, metadata io.Reader, targetPath string,
) error {
	if format != "test-json-v1" {
		return errors.New("unexpected metadata format")
	}
	raw, err := io.ReadAll(metadata)
	if err != nil {
		return err
	}
	var record portableRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", targetPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT NOT NULL);
		CREATE TABLE files (hash TEXT PRIMARY KEY, size INTEGER NOT NULL, path TEXT NOT NULL)`); err != nil {
		return err
	}
	for _, note := range record.Notes {
		if _, err := db.ExecContext(ctx, `INSERT INTO notes(body) VALUES(?)`, note); err != nil {
			return err
		}
	}
	for _, file := range record.Files {
		if _, err := db.ExecContext(ctx, `INSERT INTO files(hash,size,path) VALUES(?,?,?)`,
			file.Hash, file.Size, file.Path); err != nil {
			return err
		}
	}
	return db.Close()
}

type metadataRestorerFunc func(context.Context, string, io.Reader, string) error

func (f metadataRestorerFunc) RestoreMetadata(
	ctx context.Context, format string, metadata io.Reader, targetPath string,
) error {
	return f(ctx, format, metadata, targetPath)
}

type countingFreezer struct{ begins, ends int }

func (f *countingFreezer) Begin(context.Context) error { f.begins++; return nil }
func (f *countingFreezer) End(context.Context) error   { f.ends++; return nil }

func TestPortableMetadataCreateVerifyRestoreAndSQLiteSuccessor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := context.Background()
	base := t.TempDir()
	repo, err := backup.Init(filepath.Join(base, "repo"))
	require.NoError(err)
	contentDir := filepath.Join(base, "content")
	content := []byte("portable attachment")
	hash := pack.ComputeBlobID(content).String()
	rel := filepath.Join(hash[:2], hash)
	require.NoError(os.MkdirAll(filepath.Join(contentDir, hash[:2]), 0o700))
	require.NoError(os.WriteFile(filepath.Join(contentDir, rel), content, 0o600))
	record := portableRecord{
		Notes: []string{"alpha", "beta"},
		Files: []portableFile{{Hash: hash, Size: int64(len(content)), Path: filepath.ToSlash(rel)}},
	}
	raw, err := json.Marshal(record)
	require.NoError(err)
	stats, err := json.Marshal(portableStats{Notes: 2, Files: 1})
	require.NoError(err)
	source := &portableSource{
		raw: raw, stats: stats,
		info: &backup.ContentInfo{Refs: []backup.ContentRef{{Hash: hash, Size: int64(len(content))}}, Rows: 1},
	}
	freezer := &countingFreezer{}
	manifest, err := backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{
		MetadataSource: source, ContentDir: contentDir, Freezer: freezer, Jobs: 1,
	})
	require.NoError(err)
	assert.True(source.opened)
	assert.True(source.closed)
	assert.Equal(1, freezer.begins)
	assert.Equal(1, freezer.ends)
	require.NotNil(manifest.Metadata)
	assert.Equal("test-json-v1", manifest.Metadata.Format)
	assert.Equal(int64(len(raw)), manifest.Metadata.Bytes)
	assert.Empty(manifest.DB.Engine)
	assert.Equal(3, manifest.MinReaderVersion)

	for _, quick := range []bool{false, true} {
		verified, err := backup.Verify(ctx, repo, portableApp{}, backup.VerifyOptions{Quick: quick})
		require.NoError(err)
		assert.Empty(verified.Problems)
	}

	missingTarget := filepath.Join(base, "missing-restorer")
	_, err = backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{TargetDir: missingTarget})
	require.ErrorContains(err, "requires a MetadataRestorer")
	_, statErr := os.Stat(missingTarget)
	require.ErrorIs(statErr, os.ErrNotExist)

	incompleteTarget := filepath.Join(base, "incomplete-restorer")
	_, err = backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{
		TargetDir: incompleteTarget,
		MetadataRestorer: metadataRestorerFunc(func(
			context.Context, string, io.Reader, string,
		) error {
			return nil
		}),
	})
	require.ErrorContains(err, "before verified EOF")
	_, statErr = os.Stat(filepath.Join(incompleteTarget, "portable.db"))
	require.ErrorIs(statErr, os.ErrNotExist)

	if runtime.GOOS != "windows" {
		replacedTarget := filepath.Join(base, "replaced-target")
		heldTarget := replacedTarget + "-held"
		require.NoError(os.MkdirAll(replacedTarget, 0o700))
		var privatePath string
		_, err = backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{
			TargetDir: replacedTarget,
			MetadataRestorer: metadataRestorerFunc(func(
				ctx context.Context, format string, metadata io.Reader, targetPath string,
			) error {
				privatePath = targetPath
				if err := os.Rename(replacedTarget, heldTarget); err != nil {
					return err
				}
				if err := os.Mkdir(replacedTarget, 0o700); err != nil {
					return err
				}
				return (portableRestorer{}).RestoreMetadata(ctx, format, metadata, targetPath)
			}),
		})
		require.ErrorContains(err, "was replaced during restore")
		replacementEntries, readErr := os.ReadDir(replacedTarget)
		require.NoError(readErr)
		assert.Empty(replacementEntries)
		heldEntries, readErr := os.ReadDir(heldTarget)
		require.NoError(readErr)
		assert.Empty(heldEntries)
		assert.Equal(repo.Path("staging"), filepath.Dir(filepath.Dir(privatePath)))
		_, statErr = os.Stat(filepath.Dir(privatePath))
		require.ErrorIs(statErr, os.ErrNotExist)
	}

	overwriteTarget := filepath.Join(base, "failed-overwrite")
	require.NoError(os.MkdirAll(overwriteTarget, 0o700))
	oldDatabase := []byte("existing database remains")
	require.NoError(os.WriteFile(filepath.Join(overwriteTarget, "portable.db"), oldDatabase, 0o600))
	_, err = backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{
		TargetDir: overwriteTarget, Overwrite: true,
		MetadataRestorer: metadataRestorerFunc(func(
			_ context.Context, _ string, metadata io.Reader, _ string,
		) error {
			_, readErr := io.Copy(io.Discard, metadata)
			return errors.Join(readErr, errors.New("application import failed"))
		}),
	})
	require.ErrorContains(err, "application import failed")
	stillOld, err := os.ReadFile(filepath.Join(overwriteTarget, "portable.db"))
	require.NoError(err)
	assert.Equal(oldDatabase, stillOld)

	target := filepath.Join(base, "restored")
	result, err := backup.Restore(ctx, repo, portableApp{}, backup.RestoreOptions{
		TargetDir: target, MetadataRestorer: portableRestorer{}, Jobs: 1,
	})
	require.NoError(err)
	assert.Positive(result.DBBytes)
	restoredContent, err := os.ReadFile(filepath.Join(target, "content", rel))
	require.NoError(err)
	assert.Equal(content, restoredContent)
	db, err := sql.Open("sqlite3", filepath.Join(target, "portable.db"))
	require.NoError(err)
	defer func() { _ = db.Close() }()
	var noteCount int64
	require.NoError(db.QueryRow(`SELECT COUNT(*) FROM notes`).Scan(&noteCount))
	assert.Equal(int64(2), noteCount)

	// A legacy SQLite snapshot may follow a portable snapshot in the same
	// repository; it starts a fresh page-map keyframe while attachment-list
	// lineage still converges normally.
	legacyDir := filepath.Join(base, "legacy")
	require.NoError(os.MkdirAll(filepath.Join(legacyDir, "content"), 0o700))
	legacyDB := filepath.Join(legacyDir, "fake.db")
	legacy, err := sql.Open("sqlite3", legacyDB)
	require.NoError(err)
	_, err = legacy.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT);
		INSERT INTO notes(body) VALUES('legacy')`)
	require.NoError(err)
	require.NoError(legacy.Close())
	legacyManifest, err := backup.Create(ctx, repo, fakeApp{}, backup.CreateOptions{
		DBPath: legacyDB, ContentDir: filepath.Join(legacyDir, "content"), DataDir: legacyDir,
	})
	require.NoError(err)
	assert.Nil(legacyManifest.Metadata)
	assert.Zero(legacyManifest.DB.MapChainDepth)
}

func TestPortableMetadataCaptureClosesPartialResourcesOnce(t *testing.T) {
	ctx := context.Background()

	t.Run("metadata open error", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		reader := &closeCounter{}
		source := &portableSource{
			metadataReader: reader,
			metadataErr:    errors.New("metadata unavailable"),
			stats:          json.RawMessage(`{}`),
			info:           &backup.ContentInfo{},
		}
		repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
		require.NoError(err)
		_, err = backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{MetadataSource: source})
		require.ErrorContains(err, "metadata unavailable")
		assert.Equal(1, reader.closes)
		assert.Equal(1, source.closes)
	})

	t.Run("snapshot close error", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		source := &portableSource{
			raw:      []byte(`{}`),
			stats:    json.RawMessage(`{}`),
			info:     &backup.ContentInfo{},
			closeErr: errors.New("snapshot close failed"),
		}
		repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
		require.NoError(err)
		_, err = backup.Create(ctx, repo, portableApp{}, backup.CreateOptions{MetadataSource: source})
		require.ErrorContains(err, "snapshot close failed")
		assert.Equal(1, source.closes)
	})
}

func TestLoadManifestRejectsDualMetadataAuthority(t *testing.T) {
	require := require.New(t)
	repo, err := backup.Init(filepath.Join(t.TempDir(), "repo"))
	require.NoError(err)
	m := &backup.Manifest{
		FormatVersion:    3,
		MinReaderVersion: 3,
		CreatedAt:        time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339),
		DB:               backup.ManifestDB{Engine: "sqlite"},
		Metadata: &backup.ManifestMetadata{
			Format: "test-json-v1", Blob: pack.ComputeBlobID(nil).String(),
		},
	}
	id, err := repo.WriteManifest(m)
	require.NoError(err)
	_, err = repo.LoadManifest(id)
	require.ErrorContains(err, "also carries SQLite page-map authority")
}
