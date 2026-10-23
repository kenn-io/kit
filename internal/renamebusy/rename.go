// Package renamebusy retries renames blocked by Windows file handles.
package renamebusy

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/kit/atomicfile"
)

// Replace calls atomicfile.Replace, retrying Windows sharing violations for
// timeout with poll between attempts. It returns the last filesystem error.
func Replace(oldpath, newpath string, timeout, poll time.Duration) error {
	_, err := backoff.Retry(context.Background(), func() (struct{}, error) {
		err := atomicfile.Replace(oldpath, newpath)
		if err != nil && !fileBusy(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(backoff.NewConstantBackOff(poll)),
		backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(timeout))
	if err != nil {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
}
