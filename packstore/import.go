package packstore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path"
	"strings"
	"time"

	"go.kenn.io/kit/pack"
)

// ImportSelection identifies one catalog-authorized entry in a source pack.
// Its metadata must exactly match the source pack's authoritative footer.
type ImportSelection struct {
	Hash      Hash
	RawLen    int64
	Offset    uint64
	StoredLen uint64
	Flags     uint8
}

// ImportPack describes one immutable source pack and the entries authorized
// for import. SourcePath is read-only; importing never moves source bytes.
type ImportPack struct {
	PackID     string
	SourcePath string
	Selections []ImportSelection
}

// FallbackReason identifies a compatibility limit that requires loose restore.
type FallbackReason string

const (
	// FallbackPackContainerLimit declines every selection in an oversized pack.
	FallbackPackContainerLimit FallbackReason = "pack_container_limit"
	// FallbackPackFooterLimit declines every selection when its footer is too large.
	FallbackPackFooterLimit FallbackReason = "pack_footer_limit"
	// FallbackPackEntryCountLimit declines every selection when it has too many entries.
	FallbackPackEntryCountLimit FallbackReason = "pack_entry_count_limit"
	// FallbackPackEncoding declines every selection when pack settings are unsupported.
	FallbackPackEncoding FallbackReason = "pack_encoding"
	// FallbackBlobLimit declines one selected entry that exceeds the blob ceiling.
	FallbackBlobLimit FallbackReason = "blob_limit"
)

// ImportFallback records content that must be restored loose. Hash is empty
// when the reason applies to the whole pack.
type ImportFallback struct {
	PackID string
	Hash   Hash
	Reason FallbackReason
}

// ImportOptions configures compatibility checks for the target store.
type ImportOptions struct {
	Limits    Limits
	CreatedAt time.Time
}

// ImportStats summarizes the compatible subset selected for import.
type ImportStats struct {
	PackedPacks int
	PackedBlobs int
	Fallbacks   []ImportFallback
}

type preparedImportPack struct {
	pack       ImportPack
	entries    []pack.Entry
	selections []ImportSelection
}

// PreparedImport is a verified, authority-free import plan. A prepared hash is
// not readable through a Store until a later catalog commit grants authority.
type PreparedImport struct {
	packedHashes []Hash
	stats        ImportStats
	packs        []preparedImportPack
	createdAt    time.Time
}

// PackedHashes returns a copy of the selected hashes verified as compatible.
func (p *PreparedImport) PackedHashes() []Hash {
	if p == nil {
		return nil
	}
	return append([]Hash(nil), p.packedHashes...)
}

// Stats returns a copy of the preparation summary.
func (p *PreparedImport) Stats() ImportStats {
	if p == nil {
		return ImportStats{}
	}
	stats := p.stats
	stats.Fallbacks = append([]ImportFallback(nil), p.stats.Fallbacks...)
	return stats
}

// PrepareImport validates source pack compatibility and verifies every
// selected entry that fits the target's configured bounds. It does not grant
// catalog authority. Durable publication and catalog commit are separate
// operations so applications can preserve publish-before-authority ordering.
func PrepareImport(
	ctx context.Context,
	target *os.Root,
	contentDir string,
	packs []ImportPack,
	opts ImportOptions,
) (*PreparedImport, error) {
	if err := validateImportInputs(target, contentDir, packs, opts); err != nil {
		return nil, err
	}
	prepared := &PreparedImport{createdAt: opts.CreatedAt}
	seenPacks := make(map[string]struct{}, len(packs))
	seenHashes := make(map[Hash]struct{})
	for _, candidate := range packs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, duplicate := seenPacks[candidate.PackID]; duplicate {
			return nil, fmt.Errorf("packstore: duplicate import pack id %q", candidate.PackID)
		}
		seenPacks[candidate.PackID] = struct{}{}

		reader, err := OpenMaintenancePack(candidate.SourcePath, opts.Limits)
		if err != nil {
			if reason, compatible := importFallbackReason(err); compatible {
				if reason != FallbackPackEncoding {
					if verifyErr := verifyLimitedImportPack(candidate, opts.Limits); verifyErr != nil {
						return nil, fmt.Errorf("verify limited import pack %s: %w", candidate.PackID, verifyErr)
					}
				}
				prepared.stats.Fallbacks = append(prepared.stats.Fallbacks, ImportFallback{
					PackID: candidate.PackID,
					Reason: reason,
				})
				continue
			}
			return nil, fmt.Errorf("prepare import pack %s: %w", candidate.PackID, err)
		}

		packPlan, err := prepareImportPack(ctx, reader, candidate, opts.Limits, seenHashes, prepared)
		closeErr := reader.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close import pack %s: %w", candidate.PackID, closeErr)
		}
		if len(packPlan.selections) > 0 {
			prepared.packs = append(prepared.packs, packPlan)
			prepared.stats.PackedPacks++
		}
	}
	return prepared, nil
}

// verifyLimitedImportPack proves that a configured limit, rather than damaged
// structure, caused preflight to decline a pack. The verifier ignores only the
// container size because parsing does not allocate the container payload. It
// raises footer and entry ceilings to at least Kit's normal maintenance bounds,
// which is enough to classify ordinary Kit-produced packs against a stricter
// target without allowing an attacker-selected format maximum allocation. A
// source beyond these verification bounds fails closed unless the caller has
// explicitly configured and accepted the larger allocation contract.
func verifyLimitedImportPack(candidate ImportPack, configured Limits) error {
	verification := configured
	verification.PackBytes = math.MaxInt64
	verification.FooterBytes = max(verification.FooterBytes, defaultFooterBytes)
	verification.PackEntries = max(verification.PackEntries, defaultPackEntries)
	reader, err := OpenMaintenancePack(candidate.SourcePath, verification)
	if err != nil {
		var limitErr *LimitError
		if errors.As(err, &limitErr) {
			return fmt.Errorf("%w: source exceeds bounded import verification: %v", pack.ErrCorrupt, err)
		}
		return err
	}
	_, validationErr := indexImportSelections(reader.Entries(), candidate)
	closeErr := reader.Close()
	if validationErr != nil {
		return validationErr
	}
	if closeErr != nil {
		return fmt.Errorf("close bounded import verification: %w", closeErr)
	}
	return nil
}

func validateImportInputs(target *os.Root, contentDir string, packs []ImportPack, opts ImportOptions) error {
	if target == nil {
		return fmt.Errorf("packstore: import target is nil")
	}
	if contentDir == "" || contentDir == ".." || path.IsAbs(contentDir) ||
		path.Clean(contentDir) != contentDir || strings.Contains(contentDir, `\`) ||
		strings.HasPrefix(contentDir, "../") {
		return fmt.Errorf("packstore: unsafe import content directory %q", contentDir)
	}
	if err := validateLimits(opts.Limits); err != nil {
		return err
	}
	if opts.CreatedAt.IsZero() {
		return fmt.Errorf("packstore: import creation time is zero")
	}
	for _, candidate := range packs {
		if !pack.IsValidPackID(candidate.PackID) {
			return fmt.Errorf("packstore: invalid import pack id %q", candidate.PackID)
		}
		if candidate.SourcePath == "" {
			return fmt.Errorf("packstore: import pack %s has empty source path", candidate.PackID)
		}
		if len(candidate.Selections) == 0 {
			return fmt.Errorf("packstore: import pack %s has no selections", candidate.PackID)
		}
		for _, selection := range candidate.Selections {
			if err := selection.Hash.Validate(); err != nil {
				return err
			}
			if selection.RawLen < 0 {
				return fmt.Errorf("packstore: negative selected raw length for %s", selection.Hash)
			}
		}
	}
	return nil
}

func importFallbackReason(err error) (FallbackReason, bool) {
	if errors.Is(err, pack.ErrUnsupportedVersion) || errors.Is(err, errUnsupportedMaintenanceEncoding) {
		return FallbackPackEncoding, true
	}
	var limitErr *LimitError
	if !errors.As(err, &limitErr) {
		return "", false
	}
	switch limitErr.Dimension {
	case LimitPackContainerBytes:
		return FallbackPackContainerLimit, true
	case LimitPackFooterBytes:
		return FallbackPackFooterLimit, true
	case LimitPackEntryCount:
		return FallbackPackEntryCountLimit, true
	default:
		return "", false
	}
}

func prepareImportPack(
	ctx context.Context,
	reader *MaintenancePackReader,
	candidate ImportPack,
	limits Limits,
	seenHashes map[Hash]struct{},
	prepared *PreparedImport,
) (preparedImportPack, error) {
	entries := reader.Entries()
	authoritative, err := indexImportSelections(entries, candidate)
	if err != nil {
		return preparedImportPack{}, err
	}
	ownedCandidate := candidate
	ownedCandidate.Selections = append([]ImportSelection(nil), candidate.Selections...)
	plan := preparedImportPack{pack: ownedCandidate, entries: entries}
	for _, selection := range candidate.Selections {
		if err := ctx.Err(); err != nil {
			return preparedImportPack{}, err
		}
		if _, duplicate := seenHashes[selection.Hash]; duplicate {
			return preparedImportPack{}, fmt.Errorf("%w %s across import selections", ErrDuplicateHash, selection.Hash)
		}
		seenHashes[selection.Hash] = struct{}{}
		entry := authoritative[selection.Hash]
		if entry.RawLen > uint64(limits.BlobBytes) || entry.StoredLen > uint64(limits.BlobBytes) { //nolint:gosec // limits are non-negative
			prepared.stats.Fallbacks = append(prepared.stats.Fallbacks, ImportFallback{
				PackID: candidate.PackID,
				Hash:   selection.Hash,
				Reason: FallbackBlobLimit,
			})
			continue
		}
		if _, err := reader.ReadBlob(selection.Hash); err != nil {
			return preparedImportPack{}, fmt.Errorf("verify selected blob %s in pack %s: %w", selection.Hash, candidate.PackID, err)
		}
		plan.selections = append(plan.selections, selection)
		prepared.packedHashes = append(prepared.packedHashes, selection.Hash)
		prepared.stats.PackedBlobs++
	}
	return plan, nil
}

func indexImportSelections(entries []pack.Entry, candidate ImportPack) (map[Hash]pack.Entry, error) {
	authoritative := make(map[Hash]pack.Entry, len(entries))
	for _, entry := range entries {
		hash, err := ParseHash(entry.ID.String())
		if err != nil {
			return nil, fmt.Errorf("prepare import pack %s footer: %w", candidate.PackID, err)
		}
		authoritative[hash] = entry
	}
	seen := make(map[Hash]struct{}, len(candidate.Selections))
	for _, selection := range candidate.Selections {
		if _, duplicate := seen[selection.Hash]; duplicate {
			return nil, fmt.Errorf("%w %s in import pack %s", ErrDuplicateHash, selection.Hash, candidate.PackID)
		}
		seen[selection.Hash] = struct{}{}
		entry, ok := authoritative[selection.Hash]
		if !ok {
			return nil, fmt.Errorf("%w: selected blob %s is absent from pack %s footer", pack.ErrCorrupt, selection.Hash, candidate.PackID)
		}
		if selection.RawLen != int64(entry.RawLen) || selection.Offset != entry.Offset ||
			selection.StoredLen != entry.StoredLen || selection.Flags != uint8(entry.Flags) { //nolint:gosec // format caps RawLen below MaxInt64
			return nil, fmt.Errorf("%w: selected metadata for %s does not match pack %s footer", pack.ErrCorrupt, selection.Hash, candidate.PackID)
		}
	}
	return authoritative, nil
}
