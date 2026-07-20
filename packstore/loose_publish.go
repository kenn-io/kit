package packstore

import (
	"errors"
	"fmt"
)

// publishLooseFileNoReplace first uses a hard link so successful publication
// leaves private staging available for ordinary cleanup. Filesystems without
// hard-link support fall back to a platform atomic no-replace rename; a
// check-then-rename or visible copy would expose incomplete bytes or clobber a
// concurrent publisher.
func publishLooseFileNoReplace(staging, final string) error {
	linkErr := linkLoosePublicationFile(staging, final)
	if linkErr == nil {
		return nil
	}
	if renameErr := renameLoosePublicationNoReplace(staging, final); renameErr != nil {
		return errors.Join(
			fmt.Errorf("hard-link loose publication: %w", linkErr),
			fmt.Errorf("no-replace rename loose publication: %w", renameErr),
		)
	}
	return nil
}
