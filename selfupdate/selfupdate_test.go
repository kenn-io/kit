package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testHash64   = "abc123def456789012345678901234567890123456789012345678901234abcd"
	testHashAAAA = "abc123def456789012345678901234567890123456789012345678901234aaaa"
	testHashBBBB = "abc123def456789012345678901234567890123456789012345678901234bbbb"
)

func TestCheckFindsUpdateAndChecksumAsset(t *testing.T) {
	t.Parallel()

	var checksumRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kenn/tool/releases/latest":
			_ = json.NewEncoder(w).Encode(Release{
				TagName: "v1.2.0",
				Body:    "ignored",
				Assets: []Asset{
					{Name: "tool_1.2.0_linux_amd64.tar.gz", Size: 123, BrowserDownloadURL: "https://example.invalid/tool"},
					{Name: "SHA256SUMS", BrowserDownloadURL: "http://" + r.Host + "/SHA256SUMS"},
					{Name: "tool_1.2.0_linux_amd64.tar.gz.sha256.sig", BrowserDownloadURL: "https://example.invalid/tool.sig"},
				},
			})
		case "/SHA256SUMS":
			checksumRequests.Add(1)
			_, _ = fmt.Fprintf(w, "%s  tool_1.2.0_linux_amd64.tar.gz\n", testHash64)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "v1.1.0",
		CacheDir:         t.TempDir(),
		GitHubAPIBaseURL: server.URL,
		Clock:            func() time.Time { return time.Unix(100, 0) },
	}

	info, err := client.Check(context.Background(), CheckOptions{GOOS: "linux", GOARCH: "amd64"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info == nil {
		t.Fatal("expected update info")
	}
	if info.CurrentVersion != "v1.1.0" || info.LatestVersion != "v1.2.0" {
		t.Fatalf("unexpected versions: %+v", info)
	}
	if info.AssetName != "tool_1.2.0_linux_amd64.tar.gz" {
		t.Fatalf("asset = %q", info.AssetName)
	}
	if info.SignatureURL != "https://example.invalid/tool.sig" {
		t.Fatalf("signature URL = %q", info.SignatureURL)
	}
	if info.Checksum != testHash64 {
		t.Fatalf("checksum = %q", info.Checksum)
	}
	if checksumRequests.Load() != 1 {
		t.Fatalf("checksum requests = %d", checksumRequests.Load())
	}
}

func TestCheckUsesReleaseBodyChecksumFallback(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(Release{
			TagName: "v1.2.0",
			Body:    fmt.Sprintf("%s  custom.tgz", testHashAAAA),
			Assets: []Asset{
				{Name: "custom.tgz", Size: 55, BrowserDownloadURL: "https://example.invalid/custom"},
			},
		})
	}))
	defer server.Close()

	client := Client{
		Owner:            "kenn",
		Repo:             "tool",
		BinaryName:       "tool",
		CurrentVersion:   "dev",
		GitHubAPIBaseURL: server.URL,
		AssetName: func(AssetRequest) string {
			return "custom.tgz"
		},
	}

	info, err := client.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if info.Checksum != testHashAAAA {
		t.Fatalf("checksum = %q", info.Checksum)
	}
	if !info.IsDevBuild {
		t.Fatal("expected dev build")
	}
}

func TestCheckCache(t *testing.T) {
	t.Parallel()

	now := time.Unix(1000, 0)
	tests := []struct {
		name           string
		currentVersion string
		isDevBuild     bool
		cachedVersion  string
		cacheAge       time.Duration
		wantInfo       bool
		wantDone       bool
		wantCacheOnly  bool
	}{
		{
			name:           "valid cache no update available",
			currentVersion: "v1.0.0",
			cachedVersion:  "v1.0.0",
			cacheAge:       30 * time.Minute,
			wantDone:       true,
		},
		{
			name:           "valid cache update available triggers fresh fetch",
			currentVersion: "v1.0.0",
			cachedVersion:  "v1.1.0",
			cacheAge:       30 * time.Minute,
			wantDone:       false,
		},
		{
			name:           "dev build returns cache-only update info",
			currentVersion: "0.16.1-2-g75d300a",
			isDevBuild:     true,
			cachedVersion:  "v1.0.0",
			cacheAge:       5 * time.Minute,
			wantInfo:       true,
			wantDone:       true,
			wantCacheOnly:  true,
		},
		{
			name:           "parseable dev build at cached release does not downgrade",
			currentVersion: "v1.0.0-2-g75d300a",
			isDevBuild:     true,
			cachedVersion:  "v1.0.0",
			cacheAge:       5 * time.Minute,
			wantDone:       true,
		},
		{
			name:           "expired cache",
			currentVersion: "v1.0.0",
			cachedVersion:  "v1.0.0",
			cacheAge:       2 * time.Hour,
			wantDone:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cacheDir := t.TempDir()
			data, err := json.Marshal(cachedCheck{
				CheckedAt: now.Add(-tt.cacheAge),
				Version:   tt.cachedVersion,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cacheDir, defaultCacheFileName), data, 0o600); err != nil {
				t.Fatal(err)
			}
			c := Client{
				BinaryName: "tool",
				CacheDir:   cacheDir,
				Clock:      func() time.Time { return now },
			}
			cleanVersion := strings.TrimPrefix(tt.currentVersion, "v")
			info, done := c.checkCache(tt.currentVersion, cleanVersion, tt.isDevBuild)
			if done != tt.wantDone {
				t.Fatalf("done = %v, want %v", done, tt.wantDone)
			}
			if (info != nil) != tt.wantInfo {
				t.Fatalf("info nil = %v, wantInfo %v", info == nil, tt.wantInfo)
			}
			if info != nil && info.NeedsRefetch() != tt.wantCacheOnly {
				t.Fatalf("NeedsRefetch = %v, want %v", info.NeedsRefetch(), tt.wantCacheOnly)
			}
		})
	}
}

func TestSaveCacheFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permissions not enforced on Windows")
	}
	t.Parallel()

	cacheDir := t.TempDir()
	c := Client{
		CacheDir: cacheDir,
		Clock:    func() time.Time { return time.Unix(1, 0) },
	}
	if err := c.saveCache("v1.0.0"); err != nil {
		t.Fatalf("saveCache: %v", err)
	}
	info, err := os.Stat(filepath.Join(cacheDir, defaultCacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("cache file mode = %04o, want 0600", got)
	}
}

func TestInstallDownloadsVerifiesAndInstalls(t *testing.T) {
	t.Parallel()

	binaryName := "tool"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "tool_1.2.0_linux_amd64.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{{Name: binaryName, Content: "new-binary", Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	payload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.2.0",
		Asset:    filepath.Base(archivePath),
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	publicKey, signature := signPayload(t, payload)
	archiveBytes, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			_, _ = w.Write(archiveBytes)
		case "/archive.sig":
			_, _ = w.Write(signature)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	dstPath := filepath.Join(tmpDir, binaryName)
	var lastProgress int64
	c := Client{
		Owner:             "kenn",
		Repo:              "tool",
		BinaryName:        "tool",
		TrustedPublicKeys: []ed25519.PublicKey{publicKey},
	}
	err = c.Install(context.Background(), &Info{
		DownloadURL:   server.URL + "/archive",
		SignatureURL:  server.URL + "/archive.sig",
		AssetName:     filepath.Base(archivePath),
		LatestVersion: "v1.2.0",
		Size:          int64(len(archiveBytes)),
		Checksum:      checksum,
	}, InstallOptions{
		DestinationPath: dstPath,
		Progress: func(downloaded, total int64) {
			lastProgress = downloaded
			if total != int64(len(archiveBytes)) {
				t.Fatalf("progress total = %d", total)
			}
		},
	})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("installed content = %q", got)
	}
	if lastProgress != int64(len(archiveBytes)) {
		t.Fatalf("last progress = %d", lastProgress)
	}
}

func TestInstallRefusesUnverifiedOrCachedInfo(t *testing.T) {
	t.Parallel()

	c := Client{BinaryName: "tool"}
	if err := c.Install(context.Background(), &Info{AssetName: "tool.tar.gz"}, InstallOptions{}); err == nil {
		t.Fatal("expected missing checksum error")
	}
	if err := c.Install(context.Background(), &Info{cacheOnly: true}, InstallOptions{}); err == nil {
		t.Fatal("expected cache-only error")
	}
}

func TestInstallArchiveRequiresSignatureByDefault(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "tool.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{{Name: "tool", Content: "content", Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	dstPath := filepath.Join(tmpDir, "dest", "tool")
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{}); err != nil {
		if !strings.Contains(err.Error(), "requires a trusted public key") {
			t.Fatalf("error = %v", err)
		}
	} else {
		t.Fatal("expected missing signature verification error")
	}
	if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{AllowUnsignedChecksums: true}); err != nil {
		t.Fatalf("InstallArchive: %v", err)
	}
}

func TestInstallRequiresSignatureBeforeArchiveDownloadByDefault(t *testing.T) {
	t.Parallel()

	var archiveRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		archiveRequests.Add(1)
		_, _ = w.Write([]byte("archive"))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool"}
	err := c.Install(context.Background(), &Info{
		LatestVersion: "v1.0.0",
		DownloadURL:   server.URL,
		AssetName:     "tool.tar.gz",
		Checksum:      strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "trusted public key is required") {
		t.Fatalf("error = %v", err)
	}
	if archiveRequests.Load() != 0 {
		t.Fatalf("archive was downloaded before signature verification")
	}
}

func TestInstallRejectsUnsafeAssetName(t *testing.T) {
	t.Parallel()

	c := Client{BinaryName: "tool"}
	err := c.Install(context.Background(), &Info{
		DownloadURL: "https://example.invalid/archive",
		AssetName:   "../outside.tar.gz",
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "invalid asset name") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallVerifiesSignatureBeforeArchiveDownload(t *testing.T) {
	t.Parallel()

	var archiveRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			archiveRequests.Add(1)
			_, _ = w.Write([]byte("archive"))
		case "/archive.sig":
			_, _ = w.Write([]byte("not-a-signature"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := Client{
		Owner:             "kenn",
		Repo:              "tool",
		BinaryName:        "tool",
		TrustedPublicKeys: []ed25519.PublicKey{publicKey},
	}
	err = c.Install(context.Background(), &Info{
		LatestVersion: "v1.0.0",
		DownloadURL:   server.URL + "/archive",
		SignatureURL:  server.URL + "/archive.sig",
		AssetName:     "tool.tar.gz",
		Checksum:      strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "invalid format") {
		t.Fatalf("error = %v", err)
	}
	if archiveRequests.Load() != 0 {
		t.Fatalf("archive was downloaded before signature verification")
	}
}

func TestInstallRejectsMismatchedInfoRepository(t *testing.T) {
	t.Parallel()

	c := Client{
		Owner:      "kenn",
		Repo:       "tool",
		BinaryName: "tool",
	}
	err := c.Install(context.Background(), &Info{
		Owner:       "other",
		Repo:        "tool",
		DownloadURL: "https://example.invalid/archive",
		AssetName:   "tool.tar.gz",
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "does not match client owner") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallRejectsDownloadLargerThanExpected(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("too large"))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool", AllowUnsignedChecksums: true}
	err := c.Install(context.Background(), &Info{
		DownloadURL: server.URL,
		AssetName:   "tool.tar.gz",
		Size:        3,
		Checksum:    strings.Repeat("0", 64),
	}, InstallOptions{DestinationPath: filepath.Join(t.TempDir(), "tool")})
	if err == nil || !strings.Contains(err.Error(), "exceeded expected size") {
		t.Fatalf("error = %v", err)
	}
}

func TestInstallArchive(t *testing.T) {
	t.Parallel()

	t.Run("zip happy path with nested binary", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.zip")
		createZip(t, archivePath, []archiveEntry{
			{Name: "tool-v1.0.0/tool", Content: "zip-binary", Mode: 0o755},
			{Name: "README.md", Content: "readme", Mode: 0o644},
		})
		checksum, err := HashFile(archivePath)
		if err != nil {
			t.Fatal(err)
		}
		payload := SignaturePayload(SignatureMetadata{
			Version:  "v1.0.0",
			Asset:    "tool",
			GOOS:     runtime.GOOS,
			GOARCH:   runtime.GOARCH,
			Checksum: checksum,
		})
		publicKey, signature := signPayload(t, payload)

		dstPath := filepath.Join(tmpDir, "dest", "tool")
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{
			ArchiveBinaryName: "tool",
			TrustedPublicKeys: []ed25519.PublicKey{publicKey},
			ChecksumSignature: signature,
			SignaturePayload:  payload,
		}); err != nil {
			t.Fatalf("InstallArchive: %v", err)
		}
		got, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "zip-binary" {
			t.Fatalf("content = %q", got)
		}
	})

	t.Run("tar.gz checksum mismatch", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "test.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "tool", Content: "content", Mode: 0o755}})
		err := InstallArchive(archivePath, strings.Repeat("0", 64), filepath.Join(tmpDir, "tool"), InstallArchiveOptions{})
		if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("walks past top-level directory named binary", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "nested.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "tool/tool", Content: "nested-binary", Mode: 0o755}})
		checksum, err := HashFile(archivePath)
		if err != nil {
			t.Fatal(err)
		}

		dstPath := filepath.Join(tmpDir, "dest", "tool")
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := InstallArchive(archivePath, checksum, dstPath, InstallArchiveOptions{
			ArchiveBinaryName:      "tool",
			AllowUnsignedChecksums: true,
		}); err != nil {
			t.Fatalf("InstallArchive: %v", err)
		}
		got, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "nested-binary" {
			t.Fatalf("content = %q", got)
		}
	})
}

func TestInstallArchiveRejectsReplaySignature(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "test.zip")
	createZip(t, archivePath, []archiveEntry{{Name: "tool", Content: "content", Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}

	oldPayload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.0.0",
		Asset:    "tool",
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	newPayload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.1.0",
		Asset:    "tool",
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	publicKey, signature := signPayload(t, oldPayload)

	err = InstallArchive(archivePath, checksum, filepath.Join(tmpDir, "tool"), InstallArchiveOptions{
		ArchiveBinaryName: "tool",
		TrustedPublicKeys: []ed25519.PublicKey{publicKey},
		ChecksumSignature: signature,
		SignaturePayload:  newPayload,
	})
	if err == nil || !strings.Contains(err.Error(), "signature verification failed") {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractTarGzAndZipRejectTraversal(t *testing.T) {
	t.Parallel()

	t.Run("tar.gz traversal", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "malicious.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "../pwned", Content: "owned", Mode: 0o644}})
		err := ExtractTarGz(archivePath, filepath.Join(tmpDir, "extract"))
		if err == nil {
			t.Fatal("expected traversal error")
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "pwned")); !os.IsNotExist(err) {
			t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
		}
	})

	t.Run("zip traversal", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "malicious.zip")
		createZip(t, archivePath, []archiveEntry{{Name: "../pwned", Content: "owned", Mode: 0o644}})
		err := ExtractZip(archivePath, filepath.Join(tmpDir, "extract"))
		if err == nil {
			t.Fatal("expected traversal error")
		}
		if _, err := os.Stat(filepath.Join(tmpDir, "pwned")); !os.IsNotExist(err) {
			t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
		}
	})
}

func TestExtractArchivesRejectPreexistingSymlinkPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevated privileges on Windows")
	}
	t.Parallel()

	t.Run("tar.gz", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "symlink-path.tar.gz")
		createTarGz(t, archivePath, []archiveEntry{{Name: "link/payload", Content: "owned", Mode: 0o644}})
		extractDir := filepath.Join(tmpDir, "extract")
		outsideDir := filepath.Join(tmpDir, "outside")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(extractDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, filepath.Join(extractDir, "link")); err != nil {
			t.Fatal(err)
		}
		if err := ExtractTarGz(archivePath, extractDir); err == nil {
			t.Fatal("expected symlink path error")
		}
		if _, err := os.Stat(filepath.Join(outsideDir, "payload")); !os.IsNotExist(err) {
			t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
		}
	})

	t.Run("zip", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		archivePath := filepath.Join(tmpDir, "symlink-path.zip")
		createZip(t, archivePath, []archiveEntry{{Name: "link/payload", Content: "owned", Mode: 0o644}})
		extractDir := filepath.Join(tmpDir, "extract")
		outsideDir := filepath.Join(tmpDir, "outside")
		if err := os.MkdirAll(outsideDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(extractDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outsideDir, filepath.Join(extractDir, "link")); err != nil {
			t.Fatal(err)
		}
		if err := ExtractZip(archivePath, extractDir); err == nil {
			t.Fatal("expected symlink path error")
		}
		if _, err := os.Stat(filepath.Join(outsideDir, "payload")); !os.IsNotExist(err) {
			t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
		}
	})
}

func TestExtractTarGzSkipsSymlink(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "symlink.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{
		{Name: "evil-link", LinkName: "/etc/passwd", TypeFlag: tar.TypeSymlink},
		{Name: "normal.txt", Content: "ok", Mode: 0o644},
	})
	extractDir := filepath.Join(tmpDir, "extract")
	if err := ExtractTarGz(archivePath, extractDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extractDir, "evil-link")); !os.IsNotExist(err) {
		t.Fatalf("symlink exists or stat failed unexpectedly: %v", err)
	}
}

func TestExtractTarGzMasksDangerousModeBits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits not meaningful on Windows")
	}
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "modes.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{
		{Name: "tool", Content: "ok", Mode: 0o4755},
	})
	extractDir := filepath.Join(tmpDir, "extract")
	if err := ExtractTarGz(archivePath, extractDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	info, err := os.Stat(filepath.Join(extractDir, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode(); got&os.ModeSetuid != 0 {
		t.Fatalf("setuid bit preserved: mode=%v", got)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("permission bits = %04o, want 0755", got)
	}
}

func TestExtractTarGzExtractsLegacyRegularFiles(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "legacy-regular.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{
		{Name: "tool", Content: "legacy", Mode: 0o755, TypeFlag: tar.TypeRegA},
	})
	extractDir := filepath.Join(tmpDir, "extract")
	if err := ExtractTarGz(archivePath, extractDir); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(extractDir, "tool"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "legacy" {
		t.Fatalf("content = %q", got)
	}
}

func TestFetchChecksumFromFileLimitsResponseSize(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", maxChecksumBytes+1)), maxChecksumBytes+1))
	}))
	defer server.Close()

	c := Client{BinaryName: "tool"}
	if _, err := c.fetchChecksumFromFile(context.Background(), server.URL, "tool.tar.gz"); err == nil {
		t.Fatal("expected oversized checksum response error")
	}
}

func TestSanitizeArchivePath(t *testing.T) {
	t.Parallel()

	destDir := t.TempDir()
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"normal", "tool", false},
		{"nested", "bin/tool", false},
		{"absolute", "/etc/passwd", true},
		{"traversal", "../../../etc/passwd", true},
		{"hidden traversal", "foo/../../../etc/passwd", true},
		{"dot", ".", false},
		{"double dot", "..", true},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := SanitizeArchivePath(destDir, tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestInstallBinary(t *testing.T) {
	t.Parallel()

	t.Run("sets executable mode", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Unix mode bits not meaningful on Windows")
		}
		t.Parallel()
		tmpDir := t.TempDir()
		srcPath := filepath.Join(tmpDir, "src")
		dstPath := filepath.Join(tmpDir, "dst")
		if err := os.WriteFile(srcPath, []byte("binary"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := InstallBinary(srcPath, dstPath); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(dstPath)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o755 {
			t.Fatalf("mode = %04o, want 0755", got)
		}
	})

	t.Run("preserves destination on missing source", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		dstPath := filepath.Join(tmpDir, "tool")
		if err := os.WriteFile(dstPath, []byte("original"), 0o755); err != nil {
			t.Fatal(err)
		}
		err := InstallBinary(filepath.Join(tmpDir, "missing"), dstPath)
		if err == nil {
			t.Fatal("expected missing source error")
		}
		got, err := os.ReadFile(dstPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "original" {
			t.Fatalf("content = %q", got)
		}
		if _, err := os.Stat(dstPath + ".new"); !os.IsNotExist(err) {
			t.Fatalf("staging file exists or stat failed unexpectedly: %v", err)
		}
	})

	t.Run("never missing during unix update", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows moves the running binary aside before replacement")
		}
		t.Parallel()
		tmpDir := t.TempDir()
		srcPath := filepath.Join(tmpDir, "src")
		dstPath := filepath.Join(tmpDir, "tool")
		if err := os.WriteFile(srcPath, []byte("new"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dstPath, []byte("old"), 0o755); err != nil {
			t.Fatal(err)
		}

		var observations, missing atomic.Uint64
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := os.Stat(dstPath); os.IsNotExist(err) {
					missing.Add(1)
				}
				observations.Add(1)
			}
		}()

		for i := 0; i < 1000; i++ {
			if err := InstallBinary(srcPath, dstPath); err != nil {
				close(stop)
				<-done
				t.Fatalf("iteration %d: %v", i, err)
			}
		}
		close(stop)
		<-done

		if observations.Load() < 1000 {
			t.Skipf("observer ran only %d times", observations.Load())
		}
		if missing.Load() > 0 {
			t.Fatalf("destination missing observations = %d", missing.Load())
		}
	})
}

func TestExtractChecksum(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		assetName string
		want      string
	}{
		{"standard", fmt.Sprintf("%s  tool_darwin_arm64.tar.gz", testHash64), "tool_darwin_arm64.tar.gz", testHash64},
		{"uppercase", "ABC123DEF456789012345678901234567890123456789012345678901234ABCD  tool_linux_amd64.tar.gz", "tool_linux_amd64.tar.gz", testHash64},
		{"multiline", fmt.Sprintf("%s  tool_linux_amd64.tar.gz\n%s  tool_darwin_arm64.tar.gz", testHashAAAA, testHashBBBB), "tool_darwin_arm64.tar.gz", testHashBBBB},
		{"no match", fmt.Sprintf("%s  tool_linux_amd64.tar.gz", testHash64), "tool_darwin_arm64.tar.gz", ""},
		{"substring filename", fmt.Sprintf("%s  tool_darwin_arm64.tar.gz.sig", testHash64), "tool_darwin_arm64.tar.gz", ""},
		{"binary star", fmt.Sprintf("%s *tool_darwin_arm64.tar.gz", testHash64), "tool_darwin_arm64.tar.gz", testHash64},
		{"trailing comment", fmt.Sprintf("%s  tool_darwin_arm64.tar.gz  # comment", testHash64), "tool_darwin_arm64.tar.gz", testHash64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ExtractChecksum(tt.body, tt.assetName); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionHelpers(t *testing.T) {
	t.Parallel()

	devTests := []struct {
		version string
		want    bool
	}{
		{"dev", true},
		{"unknown", true},
		{"", true},
		{"0.1.0", false},
		{"v0.1.0", false},
		{"0.1.0-2-gabcdef", true},
		{"v0.1.0-2-gabcdef-dirty", true},
		{"0.1.0-rc1", false},
		{"v1.0.0-beta.1", false},
	}
	for _, tt := range devTests {
		t.Run("dev/"+tt.version, func(t *testing.T) {
			t.Parallel()
			if got := IsDevBuildVersion(tt.version); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}

	newerTests := []struct {
		name   string
		v1, v2 string
		want   bool
	}{
		{"major", "1.0.0", "0.9.0", true},
		{"same", "1.0.0", "1.0.0", false},
		{"release vs prerelease", "0.4.0", "0.4.0-rc1", true},
		{"prerelease vs release", "0.4.0-rc1", "0.4.0", false},
		{"rc10 vs rc2", "0.4.0-rc10", "0.4.0-rc2", true},
		{"hash", "0.4.2", "88be010", false},
		{"dev base", "0.5.0", "0.4.0-5-gabcdef", true},
	}
	for _, tt := range newerTests {
		t.Run("newer/"+tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNewer(tt.v1, tt.v2); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDefaultAssetNameAndFormatSize(t *testing.T) {
	t.Parallel()

	name := DefaultAssetName(AssetRequest{
		BinaryName: "tool",
		Version:    "1.2.3",
		GOOS:       "linux",
		GOARCH:     "amd64",
		Extension:  ".tar.gz",
	})
	if name != "tool_1.2.3_linux_amd64.tar.gz" {
		t.Fatalf("asset name = %q", name)
	}

	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()
			if got := FormatSize(tt.bytes); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

type archiveEntry struct {
	Name     string
	Content  string
	Mode     int64
	TypeFlag byte
	LinkName string
}

func createTarGz(t *testing.T, path string, entries []archiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, entry := range entries {
		mode := entry.Mode
		if mode == 0 {
			mode = 0o644
		}
		typeFlag := entry.TypeFlag
		if typeFlag == 0 {
			typeFlag = tar.TypeReg
		}
		data := []byte(entry.Content)
		header := &tar.Header{
			Name:     entry.Name,
			Mode:     mode,
			Size:     int64(len(data)),
			Typeflag: typeFlag,
			Linkname: entry.LinkName,
		}
		if typeFlag != tar.TypeReg && typeFlag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typeFlag == tar.TypeReg || typeFlag == tar.TypeRegA {
			if _, err := tw.Write(data); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func createZip(t *testing.T, path string, entries []archiveEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.Name}
		mode := os.FileMode(entry.Mode)
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		w, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(w, bytes.NewBufferString(entry.Content)); err != nil {
			t.Fatal(err)
		}
	}
}

func signPayload(t *testing.T, payload []byte) (ed25519.PublicKey, []byte) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, ed25519.Sign(privateKey, payload)
}
