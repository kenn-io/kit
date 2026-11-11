package safefileio_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
)

func TestSecretParentACLPolicy(t *testing.T) {
	for _, tc := range []struct {
		acl    []string
		accept bool
	}{
		{nil, true},
		{[]string{"everyone deny delete"}, true},
		{[]string{"everyone deny delete", "everyone deny writeextattr"}, true},
		{[]string{"everyone allow add_file", "everyone deny delete"}, false},
		{[]string{"everyone deny delete", "everyone allow add_file", "everyone deny writeextattr"}, false},
		{[]string{"everyone deny delete", "everyone deny writeextattr", "everyone allow add_file"}, false},
	} {
		t.Run(strings.Join(tc.acl, ","), func(t *testing.T) {
			dir := privateTempDir(t)
			path := filepath.Join(dir, "token")
			require.NoError(t, os.WriteFile(path, []byte("token"), 0o600))
			for i, entry := range tc.acl {
				require.NoError(t, exec.CommandContext(t.Context(), "chmod", "+a#", strconv.Itoa(i), entry, dir).Run())
			}
			t.Cleanup(func() {
				// Cleanup runs after t.Context is canceled; remove the deny-delete ACL.
				ctx := context.Background() //nolint:usetesting // t.Context is canceled before cleanup.
				require.NoError(t, exec.CommandContext(ctx, "chmod", "-N", dir).Run())
			})
			file, err := safefileio.OpenSecretFile(t.Context(), path)
			if tc.accept {
				require.NoError(t, err)
				require.NoError(t, file.Close())
			} else {
				require.Error(t, err)
			}
		})
	}
}
