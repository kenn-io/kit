package packstore

import (
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
		return looseRepairPublishResult{KeepStaging: true}, errors.Join(
			replaceErr,
			fmt.Errorf("inspect repaired canonical path: %w", err),
		)
	}
	stagingState, err := inspectLooseRepairPath(staging, verified)
	if err != nil {
		return looseRepairPublishResult{KeepStaging: true}, errors.Join(
			replaceErr,
			fmt.Errorf("inspect verified repair staging path: %w", err),
		)
	}
	backupState, err := inspectLooseRepairPath(backup, verified)
	if err != nil {
		return looseRepairPublishResult{KeepStaging: stagingState.matches}, errors.Join(
			replaceErr,
			fmt.Errorf("inspect repair backup path: %w", err),
		)
	}

	if finalState.matches {
		return looseRepairPublishResult{Created: true}, errors.Join(
			replaceErr,
			cleanupLooseRepairBackup(backup, backupState.exists),
		)
	}

	if stagingState.matches {
		if finalState.exists {
			return looseRepairPublishResult{KeepStaging: true}, replaceErr
		}
		linkErr := linkLooseRepairRecoveryFile(staging, final)
		if linkErr != nil {
			finalAfter, inspectErr := inspectLooseRepairPath(final, verified)
			if inspectErr == nil && finalAfter.matches {
				return looseRepairPublishResult{Created: true}, errors.Join(
					replaceErr,
					cleanupLooseRepairBackup(backup, backupState.exists),
				)
			}
			return looseRepairPublishResult{KeepStaging: true}, errors.Join(
				replaceErr,
				fmt.Errorf("restore verified repair staging: %w", linkErr),
				inspectErr,
			)
		}
		return looseRepairPublishResult{Created: true}, errors.Join(
			replaceErr,
			cleanupLooseRepairBackup(backup, backupState.exists),
		)
	}

	if backupState.exists && !finalState.exists {
		linkErr := linkLooseRepairRecoveryFile(backup, final)
		if linkErr != nil {
			finalAfter, inspectErr := inspectLooseRepairPath(final, verified)
			if inspectErr != nil || !finalAfter.exists {
				return looseRepairPublishResult{}, errors.Join(
					replaceErr,
					fmt.Errorf("restore repaired canonical backup: %w", linkErr),
					inspectErr,
				)
			}
			return looseRepairPublishResult{}, replaceErr
		}
		return looseRepairPublishResult{}, errors.Join(
			replaceErr,
			cleanupLooseRepairBackup(backup, true),
		)
	}

	return looseRepairPublishResult{}, replaceErr
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
	file, err := os.CreateTemp(filepath.Dir(final), "."+filepath.Base(final)+".repair-backup-")
	if err != nil {
		return "", fmt.Errorf("reserve repair backup path: %w", err)
	}
	path := file.Name()
	if err := errors.Join(file.Close(), os.Remove(path)); err != nil {
		return "", fmt.Errorf("release reserved repair backup path: %w", err)
	}
	return path, nil
}
