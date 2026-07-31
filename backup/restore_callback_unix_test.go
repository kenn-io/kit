//go:build unix

package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreBeforePublicationRejectsInvalidPrivateOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "symlink",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.Remove(dbPath))
				outside := filepath.Join(t.TempDir(), "outside.db")
				require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
				require.NoError(t, os.Symlink(outside, dbPath))
			},
		},
		{
			name: "directory",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.Remove(dbPath))
				require.NoError(t, os.Mkdir(dbPath, 0o700))
			},
		},
		{
			name: "wal sidecar",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600))
			},
		},
		{
			name: "shm sidecar",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(dbPath+"-shm", []byte("shm"), 0o600))
			},
		},
		{
			name: "journal sidecar",
			mutate: func(t *testing.T, dbPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(dbPath+"-journal", []byte("journal"), 0o600))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			ctx := context.Background()
			r := initTestRepo(t)
			dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
			_, err := Create(ctx, r, newTestApp(), createOpts(
				dbPath, attachmentsDir, dataDir, t.TempDir(),
			))
			require.NoError(err)

			target := filepath.Join(t.TempDir(), "restore")
			original := []byte("existing database remains unpublished")
			require.NoError(os.Mkdir(target, 0o700))
			require.NoError(os.WriteFile(filepath.Join(target, newTestApp().DBFileName()), original, 0o600))
			beforeScratch := restorePublicationScratchDirs(t, r)
			_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
				TargetDir: target,
				Overwrite: true,
				BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
					tc.mutate(t, staged.DBPath)
					return nil
				},
			})
			require.Error(err)
			got, readErr := os.ReadFile(filepath.Join(target, newTestApp().DBFileName()))
			require.NoError(readErr)
			assert.Equal(original, got, "invalid callback output must fail before canonical publication")
			assert.Empty(restoreDatabaseStageFiles(t, target, newTestApp().DBFileName()))
			assert.Equal(beforeScratch, restorePublicationScratchDirs(t, r))
		})
	}
}

func TestRestoreBeforePublicationCannotWriteReplacedTargetNamespace(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(
		dbPath, attachmentsDir, dataDir, t.TempDir(),
	))
	require.NoError(err)

	parent := t.TempDir()
	target := filepath.Join(parent, "restore")
	heldTarget := filepath.Join(parent, "restore-held")
	var attackerDB string
	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target,
		BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
			require.NoError(os.Rename(target, heldTarget))
			require.NoError(os.Mkdir(target, 0o700))
			attackerDB = filepath.Join(target, filepath.Base(staged.DBPath))
			attacker, err := sql.Open("sqlite3", attackerDB)
			require.NoError(err)
			require.NoError(func() error {
				_, err := attacker.Exec("PRAGMA user_version = 99")
				return err
			}())
			require.NoError(attacker.Close())

			callbackDB, err := sql.Open("sqlite3", staged.DBPath)
			require.NoError(err)
			require.NoError(func() error {
				_, err := callbackDB.Exec("PRAGMA user_version = 1")
				return err
			}())
			require.NoError(callbackDB.Close())
			return nil
		},
	})
	require.Error(err, "the replaced target must never be accepted for later proof or publication")

	attacker, err := sql.Open("sqlite3", attackerDB)
	require.NoError(err)
	defer func() { _ = attacker.Close() }()
	var userVersion int
	require.NoError(attacker.QueryRow("PRAGMA user_version").Scan(&userVersion))
	require.Equal(99, userVersion, "callback writes must not reach the attacker-selected database")
}
