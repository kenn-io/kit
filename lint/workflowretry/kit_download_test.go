package workflowretry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKitDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix shell paths")
	}
	for _, tool := range []string{"bash", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(err)
		}
	}
	for _, fail := range []bool{false, true} {
		t.Run(strconv.FormatBool(fail), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				if fail {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Length", "13")
				if n == 1 {
					_, _ = w.Write([]byte("partial"))
					return
				}
				_, _ = w.Write([]byte("complete body"))
			}))
			defer server.Close()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o700))
			dest := filepath.Join(dir, "artifact")
			require.NoError(t, os.WriteFile(dest, []byte("original"), 0o644))
			script, err := filepath.Abs("../../.github/actions/kit-retry/kit-download")
			require.NoError(t, err)
			cmd := exec.CommandContext(t.Context(), script, server.URL, dest)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if fail {
				require.Error(t, err)
				assert.EqualValues(t, 3, attempts.Load())
			} else {
				require.NoError(t, err, "%s", out)
				assert.EqualValues(t, 2, attempts.Load())
			}
			body, err := os.ReadFile(dest)
			require.NoError(t, err)
			want := "complete body"
			if fail {
				want = "original"
			}
			assert.Equal(t, want, string(body))
			parts, err := filepath.Glob(dest + ".part.*")
			require.NoError(t, err)
			assert.Empty(t, parts)
		})
	}
}
