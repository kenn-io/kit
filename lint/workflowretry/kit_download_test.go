package workflowretry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	for _, tt := range []struct{ name, want string }{{"cut short then complete", "complete body"}, {"always fails", "original"}} {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.want == "original" {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Length", "13")
				body := "complete body"
				if attempts.Add(1) == 1 {
					body = "partial"
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "sleep"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o700))
			dest := filepath.Join(dir, "artifact")
			require.NoError(t, os.WriteFile(dest, []byte("original"), 0o644))
			cmd := exec.CommandContext(t.Context(), "../../.github/actions/kit-retry/kit-download", server.URL, dest)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			assert.Equal(t, tt.want == "original", err != nil, "%s", out)
			body, err := os.ReadFile(dest)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(body))
			parts, err := filepath.Glob(dest + ".part.*")
			require.NoError(t, err)
			assert.Empty(t, parts)
		})
	}
}
