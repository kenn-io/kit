// Package selfupdate provides reusable GitHub-release based self-update
// helpers for command-line tools.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	defaultGitHubAPIBaseURL = "https://api.github.com"
	defaultCacheFileName    = "update_check.json"
	defaultCacheDuration    = time.Hour
	defaultDevCacheDuration = 15 * time.Minute
	defaultHTTPTimeout      = 30 * time.Second
	maxChecksumBytes        = 1 << 20
	maxSignatureBytes       = 64 << 10
)

// Release represents the subset of a GitHub release response used by Client.
type Release struct {
	TagName string  `json:"tag_name"`
	Body    string  `json:"body"`
	Assets  []Asset `json:"assets"`
}

// Asset represents the subset of a GitHub release asset used by Client.
type Asset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// AssetRequest describes a platform release asset lookup.
type AssetRequest struct {
	BinaryName string
	Version    string
	GOOS       string
	GOARCH     string
	Extension  string
	Release    *Release
}

// AssetNamer builds the release asset name for a platform.
type AssetNamer func(AssetRequest) string

// Client checks and installs GitHub release updates.
type Client struct {
	Owner          string
	Repo           string
	BinaryName     string
	CurrentVersion string
	CacheDir       string

	HTTPClient *http.Client
	Clock      func() time.Time

	GitHubAPIBaseURL string
	UserAgent        string

	CacheFileName    string
	CacheDuration    time.Duration
	DevCacheDuration time.Duration

	AssetName          AssetNamer
	ChecksumAssetNames []string

	TrustedPublicKeys      []ed25519.PublicKey
	AllowUnsignedChecksums bool
}

// CheckOptions controls update discovery.
type CheckOptions struct {
	Force  bool
	GOOS   string
	GOARCH string
}

// Info contains information about an available update.
type Info struct {
	Owner          string
	Repo           string
	CurrentVersion string
	LatestVersion  string
	DownloadURL    string
	AssetName      string
	SignatureURL   string
	GOOS           string
	GOARCH         string
	Size           int64
	Checksum       string
	IsDevBuild     bool

	cacheOnly bool
}

// NeedsRefetch reports whether Info came from cache and lacks download
// metadata. Call Check again with Force before installing.
func (i *Info) NeedsRefetch() bool {
	return i != nil && i.cacheOnly
}

// InstallOptions controls verified download and installation.
type InstallOptions struct {
	DestinationPath   string
	ArchiveBinaryName string
	TempDir           string
	Progress          func(downloaded, total int64)
}

// Check checks the latest GitHub release and returns update metadata when an
// update is available. A nil Info means no update is available.
func (c Client) Check(ctx context.Context, opts CheckOptions) (*Info, error) {
	if err := c.validateCheckConfig(); err != nil {
		return nil, err
	}

	currentVersion := c.CurrentVersion
	cleanVersion := strings.TrimPrefix(currentVersion, "v")
	isDevBuild := IsDevBuildVersion(cleanVersion)

	if !opts.Force {
		if info, done := c.checkCache(currentVersion, cleanVersion, isDevBuild); done {
			return info, nil
		}
	}

	release, err := c.fetchLatestRelease(ctx)
	if err != nil {
		return nil, fmt.Errorf("check for updates: %w", err)
	}

	_ = c.saveCache(release.TagName)

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	if !shouldOfferUpdate(latestVersion, cleanVersion, isDevBuild) {
		return nil, nil
	}

	goos, goarch := platform(opts)
	assetName := c.platformAssetName(release, latestVersion, opts)
	asset, checksumsAsset, signatureAsset := c.findAssets(release.Assets, assetName)
	if asset == nil {
		return nil, fmt.Errorf("no release asset for %s/%s", goos, goarch)
	}

	var checksum string
	if checksumsAsset != nil {
		checksum, _ = c.fetchChecksumFromFile(ctx, checksumsAsset.BrowserDownloadURL, assetName)
	}
	if checksum == "" {
		checksum = ExtractChecksum(release.Body, assetName)
	}

	return &Info{
		CurrentVersion: currentVersion,
		LatestVersion:  release.TagName,
		DownloadURL:    asset.BrowserDownloadURL,
		AssetName:      asset.Name,
		SignatureURL:   assetURL(signatureAsset),
		Owner:          c.Owner,
		Repo:           c.Repo,
		GOOS:           goos,
		GOARCH:         goarch,
		Size:           asset.Size,
		Checksum:       checksum,
		IsDevBuild:     isDevBuild,
	}, nil
}

// Install downloads, verifies, extracts, and installs info.
func (c Client) Install(ctx context.Context, info *Info, opts InstallOptions) error {
	if info == nil {
		return fmt.Errorf("install: update info is nil")
	}
	if info.NeedsRefetch() {
		return fmt.Errorf("install: update info came from cache; re-check with Force before installing")
	}
	if info.Checksum == "" {
		return fmt.Errorf("no checksum for %s - refusing unverified binary", info.AssetName)
	}
	if info.DownloadURL == "" {
		return fmt.Errorf("install: download URL is empty")
	}

	tempDir, err := os.MkdirTemp(opts.TempDir, c.tempPrefix("update"))
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	archivePath := filepath.Join(tempDir, info.AssetName)
	downloadChecksum, err := c.downloadFile(ctx, info.DownloadURL, archivePath, info.Size, opts.Progress)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}

	checksumSignature, err := c.downloadChecksumSignature(ctx, info)
	if err != nil {
		return err
	}

	dstPath := opts.DestinationPath
	if dstPath == "" {
		dstPath, err = c.defaultDestinationPath()
		if err != nil {
			return err
		}
	}
	archiveBinaryName := opts.ArchiveBinaryName
	if archiveBinaryName == "" {
		archiveBinaryName = executableName(c.BinaryName)
	}

	return InstallArchive(archivePath, info.Checksum, dstPath, InstallArchiveOptions{
		ArchiveBinaryName:      archiveBinaryName,
		PrecomputedChecksum:    downloadChecksum,
		TempDir:                opts.TempDir,
		TrustedPublicKeys:      c.TrustedPublicKeys,
		ChecksumSignature:      checksumSignature,
		SignaturePayload:       c.signaturePayload(info),
		AllowUnsignedChecksums: c.AllowUnsignedChecksums,
	})
}

// InstallArchiveOptions controls installing an already downloaded archive.
type InstallArchiveOptions struct {
	ArchiveBinaryName      string
	PrecomputedChecksum    string
	TempDir                string
	TrustedPublicKeys      []ed25519.PublicKey
	ChecksumSignature      []byte
	SignaturePayload       []byte
	AllowUnsignedChecksums bool
}

// InstallArchive verifies archivePath, extracts it, and installs the target
// binary to dstPath.
func InstallArchive(archivePath, expectedChecksum, dstPath string, opts InstallArchiveOptions) error {
	if expectedChecksum == "" {
		return fmt.Errorf("empty checksum - refusing unverified binary")
	}
	if dstPath == "" {
		return fmt.Errorf("destination path is empty")
	}

	checksum := opts.PrecomputedChecksum
	if checksum == "" {
		var err error
		checksum, err = HashFile(archivePath)
		if err != nil {
			return fmt.Errorf("hash archive: %w", err)
		}
	}
	if !strings.EqualFold(checksum, expectedChecksum) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, checksum)
	}
	if err := verifyChecksumTrust(opts.SignaturePayload, opts.ChecksumSignature, opts.TrustedPublicKeys, opts.AllowUnsignedChecksums); err != nil {
		return err
	}

	extractDir, err := os.MkdirTemp(opts.TempDir, "selfupdate-extract-*")
	if err != nil {
		return fmt.Errorf("create extract dir: %w", err)
	}
	defer os.RemoveAll(extractDir)

	if strings.HasSuffix(archivePath, ".zip") {
		if err := ExtractZip(archivePath, extractDir); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	} else {
		if err := ExtractTarGz(archivePath, extractDir); err != nil {
			return fmt.Errorf("extract: %w", err)
		}
	}

	binaryName := opts.ArchiveBinaryName
	if binaryName == "" {
		binaryName = filepath.Base(dstPath)
	}
	srcPath, err := findExtractedBinary(extractDir, binaryName)
	if err != nil {
		return err
	}
	return InstallBinary(srcPath, dstPath)
}

// HashFile computes the SHA-256 hash of a file.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// InstallBinary replaces dstPath with srcPath using a staged sibling file.
func InstallBinary(srcPath, dstPath string) error {
	dstDir := filepath.Dir(dstPath)
	dstBase := filepath.Base(dstPath)

	tmpFile, err := os.CreateTemp(dstDir, "."+dstBase+".*.new")
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("stage: %w", err)
	}

	installed := false
	defer func() {
		if !installed {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := copyFile(srcPath, tmpPath); err != nil {
		return fmt.Errorf("install: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	movedAside := false
	backupPath := ""
	if runtime.GOOS == "windows" {
		backupFile, err := os.CreateTemp(dstDir, "."+dstBase+".*.old")
		if err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		backupPath = backupFile.Name()
		if err := backupFile.Close(); err != nil {
			_ = os.Remove(backupPath)
			return fmt.Errorf("backup: %w", err)
		}
		if err := os.Remove(backupPath); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
		aside, err := movePreviousAside(dstPath, backupPath)
		if err != nil {
			return err
		}
		movedAside = aside
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		if movedAside {
			if rbErr := os.Rename(backupPath, dstPath); rbErr != nil {
				return fmt.Errorf("install: %w (rollback also failed: %v)", err, rbErr)
			}
		}
		return fmt.Errorf("install: %w", err)
	}

	installed = true
	if backupPath != "" {
		_ = os.Remove(backupPath)
	}
	return nil
}

// ExtractTarGz extracts a .tar.gz archive into destDir and rejects entries
// that escape destDir. Symlinks and hardlinks are skipped.
func ExtractTarGz(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target, err := SanitizeArchivePath(absDestDir, header.Name)
		if err != nil {
			return fmt.Errorf("invalid tar entry %q: %w", header.Name, err)
		}
		if header.Typeflag == tar.TypeSymlink || header.Typeflag == tar.TypeLink {
			continue
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := ensureNoSymlinkPath(absDestDir, target); err != nil {
				return err
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := ensureNoSymlinkPath(absDestDir, target); err != nil {
				return err
			}
		case tar.TypeReg:
			outFile, err := createArchiveFile(absDestDir, target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(outFile, tr); err != nil {
				_ = outFile.Close()
				return err
			}
			if err := outFile.Close(); err != nil {
				return err
			}
			if err := os.Chmod(target, os.FileMode(header.Mode)&os.ModePerm); err != nil {
				return err
			}
		}
	}
	return nil
}

// ExtractZip extracts a .zip archive into destDir and rejects entries that
// escape destDir.
func ExtractZip(archivePath, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return fmt.Errorf("resolve dest dir: %w", err)
	}

	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer r.Close()

	for _, f := range r.File {
		target, err := SanitizeArchivePath(absDestDir, f.Name)
		if err != nil {
			return fmt.Errorf("invalid zip entry %q: %w", f.Name, err)
		}
		if f.FileInfo().IsDir() {
			if err := ensureNoSymlinkPath(absDestDir, target); err != nil {
				return err
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			if err := ensureNoSymlinkPath(absDestDir, target); err != nil {
				return err
			}
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}
		outFile, err := createArchiveFile(absDestDir, target)
		if err != nil {
			_ = rc.Close()
			return err
		}
		_, copyErr := io.Copy(outFile, rc)
		closeErr := outFile.Close()
		_ = rc.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// SanitizeArchivePath validates an archive entry path to prevent directory
// traversal.
func SanitizeArchivePath(destDir, name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path not allowed")
	}

	cleanName := filepath.Clean(name)
	if filepath.IsAbs(cleanName) {
		return "", fmt.Errorf("absolute path not allowed")
	}
	if strings.HasPrefix(cleanName, "..") ||
		strings.Contains(cleanName, string(filepath.Separator)+"..") {
		return "", fmt.Errorf("path traversal not allowed")
	}

	target := filepath.Join(destDir, cleanName)
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	absDestDir, err := filepath.Abs(destDir)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absTarget, absDestDir+string(filepath.Separator)) && absTarget != absDestDir {
		return "", fmt.Errorf("path escapes destination directory")
	}
	return target, nil
}

// ExtractChecksum parses sha256sum-style text and returns the checksum for
// assetName.
func ExtractChecksum(body, assetName string) string {
	lines := strings.Split(body, "\n")
	re := regexp.MustCompile(`(?i)[a-f0-9]{64}`)
	for _, line := range lines {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		fname := strings.TrimPrefix(fields[1], "*")
		if fname == assetName {
			if match := re.FindString(fields[0]); match != "" {
				return strings.ToLower(match)
			}
		}
	}
	return ""
}

// DefaultAssetName returns the archive naming convention used by current
// kenn-io CLIs: binary_version_goos_goarch.tar.gz, or .zip on Windows.
func DefaultAssetName(req AssetRequest) string {
	return fmt.Sprintf("%s_%s_%s_%s%s", req.BinaryName, req.Version, req.GOOS, req.GOARCH, req.Extension)
}

// IsDevBuildVersion reports whether v is a non-release or git-describe build.
func IsDevBuildVersion(v string) bool {
	v = strings.TrimPrefix(v, "v")
	if extractBaseSemver(v) == "" {
		return true
	}
	return gitDescribePattern.MatchString(v)
}

// IsNewer reports whether v1 is newer than v2 using semver comparison.
func IsNewer(v1, v2 string) bool {
	base1 := extractBaseSemver(v1)
	base2 := extractBaseSemver(v2)
	if base1 == "" || base2 == "" {
		return false
	}
	return semver.Compare(normalizeSemver(v1), normalizeSemver(v2)) > 0
}

// FormatSize formats bytes as a human-readable string.
func FormatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

type cachedCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Version   string    `json:"version"`
}

func (c Client) validateCheckConfig() error {
	if c.Owner == "" {
		return fmt.Errorf("selfupdate: owner is required")
	}
	if c.Repo == "" {
		return fmt.Errorf("selfupdate: repo is required")
	}
	if c.BinaryName == "" {
		return fmt.Errorf("selfupdate: binary name is required")
	}
	return nil
}

func (c Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

func (c Client) now() time.Time {
	if c.Clock != nil {
		return c.Clock()
	}
	return time.Now()
}

func (c Client) apiBaseURL() string {
	if c.GitHubAPIBaseURL != "" {
		return strings.TrimRight(c.GitHubAPIBaseURL, "/")
	}
	return defaultGitHubAPIBaseURL
}

func (c Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	if c.BinaryName != "" {
		return c.BinaryName + "-selfupdate"
	}
	return "go.kenn.io/kit/selfupdate"
}

func (c Client) cacheFileName() string {
	if c.CacheFileName != "" {
		return c.CacheFileName
	}
	return defaultCacheFileName
}

func (c Client) cacheDuration() time.Duration {
	if c.CacheDuration != 0 {
		return c.CacheDuration
	}
	return defaultCacheDuration
}

func (c Client) devCacheDuration() time.Duration {
	if c.DevCacheDuration != 0 {
		return c.DevCacheDuration
	}
	return defaultDevCacheDuration
}

func (c Client) checksumAssetNames() []string {
	if len(c.ChecksumAssetNames) > 0 {
		return c.ChecksumAssetNames
	}
	return []string{"SHA256SUMS", "checksums.txt"}
}

func (c Client) cachePath() string {
	if c.CacheDir == "" {
		return ""
	}
	return filepath.Join(c.CacheDir, c.cacheFileName())
}

func (c Client) loadCache() (*cachedCheck, error) {
	cachePath := c.cachePath()
	if cachePath == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}
	var cached cachedCheck
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, err
	}
	return &cached, nil
}

func (c Client) saveCache(version string) error {
	cachePath := c.cachePath()
	if cachePath == "" {
		return nil
	}
	cached := cachedCheck{
		CheckedAt: c.now(),
		Version:   version,
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(cachePath, data, 0o600)
}

func (c Client) checkCache(currentVersion, cleanVersion string, isDevBuild bool) (*Info, bool) {
	cached, err := c.loadCache()
	if err != nil {
		return nil, false
	}

	cacheWindow := c.cacheDuration()
	if isDevBuild {
		cacheWindow = c.devCacheDuration()
	}
	if c.now().Sub(cached.CheckedAt) >= cacheWindow {
		return nil, false
	}

	latestVersion := strings.TrimPrefix(cached.Version, "v")
	if isDevBuild {
		if !shouldOfferUpdate(latestVersion, cleanVersion, true) {
			return nil, true
		}
		return &Info{
			CurrentVersion: currentVersion,
			LatestVersion:  cached.Version,
			IsDevBuild:     true,
			cacheOnly:      true,
		}, true
	}
	if !IsNewer(latestVersion, cleanVersion) {
		return nil, true
	}
	return nil, false
}

func (c Client) fetchLatestRelease(ctx context.Context) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBaseURL(), c.Owner, c.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}
	return &release, nil
}

func (c Client) fetchChecksumFromFile(ctx context.Context, url, assetName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to fetch checksums: %s", resp.Status)
	}

	body, err := readLimited(resp.Body, maxChecksumBytes)
	if err != nil {
		return "", err
	}
	return ExtractChecksum(string(body), assetName), nil
}

func (c Client) downloadFile(ctx context.Context, url, dest string, totalSize int64, progress func(downloaded, total int64)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: %s", resp.Status)
	}

	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}

	hasher := sha256.New()
	writer := io.MultiWriter(out, hasher)
	var downloaded int64
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := writer.Write(buf[:n]); err != nil {
				_ = out.Close()
				return "", err
			}
			downloaded += int64(n)
			if progress != nil {
				progress(downloaded, totalSize)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			_ = out.Close()
			return "", readErr
		}
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (c Client) downloadChecksumSignature(ctx context.Context, info *Info) ([]byte, error) {
	if c.AllowUnsignedChecksums {
		return nil, nil
	}
	if len(c.TrustedPublicKeys) == 0 {
		return nil, fmt.Errorf("install: trusted public key is required to verify checksum provenance")
	}
	if info.SignatureURL == "" {
		return nil, fmt.Errorf("install: checksum signature for %s is missing", info.AssetName)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.SignatureURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch checksum signature: %s", resp.Status)
	}
	return readLimited(resp.Body, maxSignatureBytes)
}

func (c Client) platformAssetName(release *Release, version string, opts CheckOptions) string {
	goos, goarch := platform(opts)
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	req := AssetRequest{
		BinaryName: c.BinaryName,
		Version:    version,
		GOOS:       goos,
		GOARCH:     goarch,
		Extension:  ext,
		Release:    release,
	}
	if c.AssetName != nil {
		return c.AssetName(req)
	}
	return DefaultAssetName(req)
}

func (c Client) findAssets(assets []Asset, assetName string) (asset *Asset, checksumsAsset *Asset, signatureAsset *Asset) {
	checksumNames := map[string]struct{}{}
	for _, name := range c.checksumAssetNames() {
		checksumNames[name] = struct{}{}
	}
	signatureNames := map[string]struct{}{
		assetName + ".sha256.sig": {},
		assetName + ".sig":        {},
	}
	for i := range assets {
		a := &assets[i]
		if a.Name == assetName {
			asset = a
		}
		if _, ok := checksumNames[a.Name]; ok {
			checksumsAsset = a
		}
		if _, ok := signatureNames[a.Name]; ok {
			signatureAsset = a
		}
	}
	return asset, checksumsAsset, signatureAsset
}

func (c Client) signaturePayload(info *Info) []byte {
	owner := info.Owner
	if owner == "" {
		owner = c.Owner
	}
	repo := info.Repo
	if repo == "" {
		repo = c.Repo
	}
	goos := info.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := info.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return SignaturePayload(SignatureMetadata{
		Owner:    owner,
		Repo:     repo,
		Version:  info.LatestVersion,
		Asset:    info.AssetName,
		GOOS:     goos,
		GOARCH:   goarch,
		Checksum: info.Checksum,
	})
}

func assetURL(asset *Asset) string {
	if asset == nil {
		return ""
	}
	return asset.BrowserDownloadURL
}

func (c Client) defaultDestinationPath() (string, error) {
	currentExe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	return filepath.Join(filepath.Dir(currentExe), executableName(c.BinaryName)), nil
}

func (c Client) tempPrefix(kind string) string {
	if c.BinaryName == "" {
		return "selfupdate-" + kind + "-*"
	}
	return c.BinaryName + "-" + kind + "-*"
}

func platform(opts CheckOptions) (string, string) {
	goos := opts.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	goarch := opts.GOARCH
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	return goos, goarch
}

func executableName(binaryName string) string {
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(binaryName), ".exe") {
		return binaryName + ".exe"
	}
	return binaryName
}

func findExtractedBinary(root, binaryName string) (string, error) {
	rootCandidate := filepath.Join(root, binaryName)
	if _, err := os.Stat(rootCandidate); err == nil {
		return rootCandidate, nil
	}

	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != binaryName {
			return nil
		}
		if found != "" {
			return fmt.Errorf("multiple binaries named %s found in archive", binaryName)
		}
		found = path
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("binary %s not found in archive", binaryName)
	}
	return found, nil
}

func movePreviousAside(dstPath, backupPath string) (bool, error) {
	if _, err := os.Stat(dstPath); err != nil {
		return false, nil
	}
	if err := os.Rename(dstPath, backupPath); err != nil {
		return false, fmt.Errorf("backup: %w", err)
	}
	return true, nil
}

func verifyChecksumTrust(payload, signature []byte, publicKeys []ed25519.PublicKey, allowUnsigned bool) error {
	if allowUnsigned {
		return nil
	}
	if len(publicKeys) == 0 {
		return fmt.Errorf("checksum signature verification requires a trusted public key")
	}
	if len(payload) == 0 {
		return fmt.Errorf("checksum signature payload is empty")
	}

	sig, err := parseSignature(signature)
	if err != nil {
		return err
	}
	for _, publicKey := range publicKeys {
		if len(publicKey) == ed25519.PublicKeySize && ed25519.Verify(publicKey, payload, sig) {
			return nil
		}
	}
	return fmt.Errorf("checksum signature verification failed")
}

// SignatureMetadata is the update metadata covered by a release signature.
type SignatureMetadata struct {
	Owner    string
	Repo     string
	Version  string
	Asset    string
	GOOS     string
	GOARCH   string
	Checksum string
}

// SignaturePayload returns the canonical payload that release signatures cover.
func SignaturePayload(m SignatureMetadata) []byte {
	lines := []string{
		"go.kenn.io/kit/selfupdate/v1",
		"owner=" + m.Owner,
		"repo=" + m.Repo,
		"version=" + m.Version,
		"asset=" + m.Asset,
		"goos=" + m.GOOS,
		"goarch=" + m.GOARCH,
		"sha256=" + strings.ToLower(m.Checksum),
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func createArchiveFile(absDestDir, target string) (*os.File, error) {
	parent := filepath.Dir(target)
	if err := ensureNoSymlinkPath(absDestDir, parent); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, err
	}
	if err := ensureNoSymlinkPath(absDestDir, parent); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("archive entry target is a symlink: %s", target)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return os.Create(target)
}

func ensureNoSymlinkPath(absDestDir, target string) error {
	rel, err := filepath.Rel(absDestDir, target)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("path escapes destination directory")
	}

	current := absDestDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive path contains symlink: %s", current)
		}
	}
	return nil
}

func parseSignature(data []byte) ([]byte, error) {
	if len(data) == ed25519.SignatureSize {
		return data, nil
	}
	text := strings.TrimSpace(string(data))
	if decoded, err := hex.DecodeString(text); err == nil && len(decoded) == ed25519.SignatureSize {
		return decoded, nil
	}
	return nil, fmt.Errorf("checksum signature has invalid format")
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("response exceeds %d byte limit", max)
	}
	return data, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func extractBaseSemver(v string) string {
	v = strings.TrimPrefix(v, "v")
	if len(v) == 0 || v[0] < '0' || v[0] > '9' {
		return ""
	}
	if !strings.Contains(v, ".") {
		return ""
	}
	if idx := strings.Index(v, "-"); idx > 0 {
		v = v[:idx]
	}
	return v
}

func shouldOfferUpdate(latestVersion, currentVersion string, isDevBuild bool) bool {
	if !isDevBuild {
		return IsNewer(latestVersion, currentVersion)
	}
	if extractBaseSemver(currentVersion) == "" {
		return true
	}
	return IsNewer(latestVersion, currentVersion)
}

var gitDescribePattern = regexp.MustCompile(`-\d+-g[0-9a-f]+(-dirty)?$`)

var prereleaseNumericPattern = regexp.MustCompile(`^([A-Za-z]+)(\d+)$`)

func normalizeSemver(v string) string {
	v = strings.TrimPrefix(v, "v")
	if gitDescribePattern.MatchString(v) {
		v = gitDescribePattern.ReplaceAllString(v, "")
	}
	if idx := strings.Index(v, "-"); idx > 0 {
		base := v[:idx]
		prerelease := normalizePrereleaseIdentifiers(v[idx+1:])
		v = base + "-" + prerelease
	}
	return "v" + v
}

func normalizePrereleaseIdentifiers(prerelease string) string {
	parts := strings.Split(prerelease, ".")
	var result []string
	for _, part := range parts {
		matches := prereleaseNumericPattern.FindStringSubmatch(part)
		if matches == nil {
			result = append(result, part)
			continue
		}
		letters, digits := matches[1], matches[2]
		if len(digits) > 1 && digits[0] == '0' {
			result = append(result, part)
			continue
		}
		result = append(result, letters, digits)
	}
	return strings.Join(result, ".")
}
