package packstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var (
	linkLooseRepairRecoveryFile = os.Link
	removeLooseRepairBackupFile = os.Remove
)

type looseRepairPublishResult struct {
	Created     bool
	KeepStaging bool
	SyncShard   bool
	SyncStaging bool
}

type looseRepairPathState struct {
	exists  bool
	matches bool
}

func inspectLooseRepairPath(path string, verified fs.FileInfo) (looseRepairPathState, error) {
	info, err := snapshotLoosePathIdentity(path)
	if errors.Is(err, fs.ErrNotExist) {
		return looseRepairPathState{}, nil
	}
	if err != nil {
		return looseRepairPathState{}, err
	}
	return looseRepairPathState{exists: true, matches: os.SameFile(info, verified)}, nil
}

func reconcileLooseRepairReplacement(
	staging string,
	final string,
	backup string,
	verified fs.FileInfo,
	replaceErr error,
) (looseRepairPublishResult, error) {
	finalState, err := inspectLooseRepairPath(final, verified)
	if err != nil {
		return looseRepairPublishResult{KeepStaging: true, SyncShard: true, SyncStaging: true}, errors.Join(
			replaceErr,
			fmt.Errorf("inspect repaired canonical path: %w", err),
		)
	}
	stagingState, err := inspectLooseRepairPath(staging, verified)
	if err != nil {
		return looseRepairPublishResult{KeepStaging: true, SyncShard: true, SyncStaging: true}, errors.Join(
			replaceErr,
			fmt.Errorf("inspect verified repair staging path: %w", err),
		)
	}
	backupState, err := inspectLooseRepairPath(backup, verified)
	if err != nil {
		return looseRepairPublishResult{
				KeepStaging: stagingState.matches,
				SyncShard:   true,
				SyncStaging: stagingState.matches,
			}, errors.Join(
				replaceErr,
				fmt.Errorf("inspect repair backup path: %w", err),
			)
	}

	if finalState.matches {
		return looseRepairPublishResult{Created: true, SyncShard: true}, errors.Join(
			replaceErr,
			cleanupLooseRepairBackup(backup, backupState.exists),
		)
	}

	if stagingState.matches {
		if finalState.exists {
			return looseRepairPublishResult{
				KeepStaging: true,
				SyncShard:   backupState.exists,
				SyncStaging: true,
			}, replaceErr
		}
		linkErr := linkLooseRepairRecoveryFile(staging, final)
		if linkErr != nil {
			finalAfter, inspectErr := inspectLooseRepairPath(final, verified)
			if inspectErr == nil && finalAfter.matches {
				return looseRepairPublishResult{Created: true, SyncShard: true}, errors.Join(
					replaceErr,
					cleanupLooseRepairBackup(backup, backupState.exists),
				)
			}
			return looseRepairPublishResult{
					KeepStaging: true,
					SyncShard:   backupState.exists || finalAfter.exists,
					SyncStaging: true,
				}, errors.Join(
					replaceErr,
					fmt.Errorf("restore verified repair staging: %w", linkErr),
					inspectErr,
				)
		}
		return looseRepairPublishResult{Created: true, SyncShard: true}, errors.Join(
			replaceErr,
			cleanupLooseRepairBackup(backup, backupState.exists),
		)
	}

	if backupState.exists && !finalState.exists {
		linkErr := linkLooseRepairRecoveryFile(backup, final)
		if linkErr != nil {
			finalAfter, inspectErr := inspectLooseRepairPath(final, verified)
			if inspectErr != nil || !finalAfter.exists {
				return looseRepairPublishResult{SyncShard: true}, errors.Join(
					replaceErr,
					fmt.Errorf("restore repaired canonical backup: %w", linkErr),
					inspectErr,
				)
			}
			return looseRepairPublishResult{SyncShard: true}, replaceErr
		}
		return looseRepairPublishResult{SyncShard: true}, errors.Join(
			replaceErr,
			cleanupLooseRepairBackup(backup, true),
		)
	}

	return looseRepairPublishResult{SyncShard: backupState.exists}, replaceErr
}

func cleanupLooseRepairBackup(path string, exists bool) error {
	if !exists {
		return nil
	}
	if err := removeLooseRepairBackupFile(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove repair backup: %w", err)
	}
	return nil
}

func newLooseRepairBackupPath(final string) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate repair backup path: %w", err)
	}
	return filepath.Join(
		filepath.Dir(final),
		"."+filepath.Base(final)+".repair-backup-"+hex.EncodeToString(suffix[:]),
	), nil
}
