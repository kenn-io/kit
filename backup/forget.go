package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"go.kenn.io/kit/pack"
)

var (
	// ErrLastSnapshot means the selection would leave no recovery points.
	ErrLastSnapshot = errors.New("backup: refusing to forget the last snapshot")
	// ErrSnapshotRequired means a retained incremental snapshot needs a selected parent.
	ErrSnapshotRequired = errors.New("backup: snapshot required by a retained snapshot")
)

// ForgetOptions selects exact recovery points to remove. Retention policy
// (for example, keeping daily or monthly snapshots) belongs to the caller.
type ForgetOptions struct {
	SnapshotIDs []string // required; duplicates are ignored
	DryRun      bool     // validate and report the selection without deleting manifests
	AllowEmpty  bool     // explicitly allow deleting the last recovery point
	ForceUnlock bool     // only controls repository lock recovery, not AllowEmpty
}

// ForgetResult reports a dependency-safe removal order. Forgotten may be
// partial on error; a directory-sync error means its last removal may not be
// durable. Neither slice reports reclaimed storage: packs and indexes stay intact.
type ForgetResult struct {
	Selected  []string
	Forgotten []string
}

// Forget removes selected snapshot manifests under the exclusive repository
// lock. It rejects the entire selection if any retained SQLite incremental
// snapshot needs a selected parent. Children are removed and directory-synced
// before parents, so interruption does not break the remaining recovery points.
// Directory durability follows pack.SyncDir's platform contract.
//
// Forget does not erase backed-up bytes or reclaim pack space. The operation
// needs no source application, database, or content directory.
func Forget(ctx context.Context, r *Repo, opts ForgetOptions) (_ *ForgetResult, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(opts.SnapshotIDs) == 0 {
		return nil, errors.New("backup: forget requires snapshot IDs")
	}
	lock, err := r.AcquireExclusiveLock("forget", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, lock.Release()) }()

	selected, err := planForget(ctx, r, opts)
	if err != nil {
		return nil, err
	}
	result := &ForgetResult{Selected: selected}
	if opts.DryRun {
		return result, nil
	}
	for _, id := range selected {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := os.Remove(r.Path(snapshotsDirName, id+manifestExt)); err != nil {
			return result, fmt.Errorf("backup: forgetting snapshot %s: %w", id, err)
		}
		result.Forgotten = append(result.Forgotten, id)
		if err := pack.SyncDir(r.Path(snapshotsDirName)); err != nil {
			return result, fmt.Errorf("backup: snapshot %s removed but directory sync failed: %w", id, err)
		}
	}
	return result, nil
}

func planForget(ctx context.Context, r *Repo, opts ForgetOptions) ([]string, error) {
	manifests, err := r.ListSnapshots()
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(manifests))
	for _, m := range manifests {
		existing[m.SnapshotID] = true
	}
	selected := make(map[string]bool, len(opts.SnapshotIDs))
	for _, id := range opts.SnapshotIDs {
		if !existing[id] {
			return nil, fmt.Errorf("backup: snapshot %q: %w", id, os.ErrNotExist)
		}
		selected[id] = true
	}
	if len(selected) == len(manifests) && !opts.AllowEmpty {
		return nil, ErrLastSnapshot
	}

	// Use the same dependency walk as restore/verify. Actual chain lengths,
	// not timestamps or the declared depth, put every child before its parent.
	depths := make(map[string]int, len(selected))
	var order []string
	for _, m := range manifests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chain, err := r.manifestChain(m)
		if err != nil {
			return nil, err
		}
		if selected[m.SnapshotID] {
			depths[m.SnapshotID] = len(chain)
			order = append(order, m.SnapshotID)
			continue
		}
		for _, dependency := range chain[1:] {
			if selected[dependency.SnapshotID] {
				return nil, fmt.Errorf("%w: %s is needed by %s", ErrSnapshotRequired, dependency.SnapshotID, m.SnapshotID)
			}
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return depths[order[i]] > depths[order[j]] })
	return order, nil
}
