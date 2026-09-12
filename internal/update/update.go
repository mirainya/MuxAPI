// Package update implements signed-by-checksum release discovery and the
// cross-platform handoff used by the admin self-update flow.
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultAPIBase  = "https://api.github.com/repos/mirainya/MuxAPI"
	cacheLifetime   = 5 * time.Minute
	requestTimeout  = 30 * time.Second
	maxPackageBytes = 512 << 20
	maxBinaryBytes  = 128 << 20
)

var ErrInProgress = errors.New("已有更新正在进行")

// ReleaseAsset is the platform package selected for the running process.
type ReleaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	Size int64  `json:"size"`
}

// Release is the subset of a GitHub release shown by the admin UI.
type Release struct {
	Version     string        `json:"version"`
	Name        string        `json:"name"`
	Notes       string        `json:"notes,omitempty"`
	URL         string        `json:"url"`
	PublishedAt time.Time     `json:"published_at"`
	Prerelease  bool          `json:"prerelease"`
	Current     bool          `json:"current"`
	Newer       bool          `json:"newer"`
	Asset       *ReleaseAsset `json:"asset,omitempty"`
}

// Catalog is returned by GET /admin/updates.
type Catalog struct {
	Current      string    `json:"current"`
	Platform     string    `json:"platform"`
	Architecture string    `json:"architecture"`
	Latest       string    `json:"latest"`
	CheckedAt    time.Time `json:"checked_at"`
	Releases     []Release `json:"releases"`
}

// ApplyResult is returned after the update helper has been launched.
type ApplyResult struct {
	Version string `json:"version"`
	Status  string `json:"status"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type githubRelease struct {
	TagName     string        `json:"tag_name"`
	Name        string        `json:"name"`
	Body        string        `json:"body"`
	HTMLURL     string        `json:"html_url"`
	PublishedAt time.Time     `json:"published_at"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []githubAsset `json:"assets"`
}

// Service owns release discovery and makes sure two administrators cannot
// start two replacement processes at the same time.
type Service struct {
	current   string
	healthURL string
	client    *http.Client
	apiBase   string

	mu       sync.Mutex
	updating bool
	cache    *Catalog
	restart  func()
}

// New creates an updater for one running binary.
func New(currentVersion, healthURL string) *Service {
	return &Service{
		current:   strings.TrimSpace(currentVersion),
		healthURL: strings.TrimSpace(healthURL),
		client:    &http.Client{Timeout: requestTimeout},
		apiBase:   defaultAPIBase,
	}
}

// SetRestartFunc registers the application's graceful shutdown hook. The
// helper starts the replacement after this function returns.
func (s *Service) SetRestartFunc(restart func()) { s.restart = restart }

// Catalog fetches the public GitHub release list. It is intentionally fetched
// only when the version menu is opened and cached briefly to avoid API limits.
func (s *Service) Catalog(ctx context.Context) (*Catalog, error) {
	s.mu.Lock()
	if s.cache != nil && time.Since(s.cache.CheckedAt) < cacheLifetime {
		copy := *s.cache
		copy.Releases = append([]Release(nil), s.cache.Releases...)
		s.mu.Unlock()
		return &copy, nil
	}
	s.mu.Unlock()

	releases, err := s.fetchReleases(ctx)
	if err != nil {
		return nil, err
	}
	catalog := &Catalog{
		Current:      displayVersion(s.current),
		Platform:     runtime.GOOS,
		Architecture: runtime.GOARCH,
		CheckedAt:    time.Now().UTC(),
		Releases:     releases,
	}
	for i := range catalog.Releases {
		catalog.Releases[i].Current = sameVersion(catalog.Current, catalog.Releases[i].Version)
		catalog.Releases[i].Newer = newerVersion(catalog.Current, catalog.Releases[i].Version)
		if catalog.Latest == "" && !catalog.Releases[i].Prerelease && catalog.Releases[i].Asset != nil {
			catalog.Latest = catalog.Releases[i].Version
		}
	}
	s.mu.Lock()
	s.cache = catalog
	s.mu.Unlock()
	return catalog, nil
}

func (s *Service) fetchReleases(ctx context.Context) ([]Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase+"/releases?per_page=30", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "MuxAPI-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("读取 GitHub 发行版失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("GitHub 发行版返回 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var raw []githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("解析 GitHub 发行版失败: %w", err)
	}
	out := make([]Release, 0, len(raw))
	for _, item := range raw {
		version := strings.TrimSpace(item.TagName)
		if item.Draft || version == "" {
			continue
		}
		asset := selectPackage(item.Assets)
		var viewAsset *ReleaseAsset
		if asset != nil {
			viewAsset = &ReleaseAsset{Name: asset.Name, URL: asset.BrowserDownloadURL, Size: asset.Size}
		}
		out = append(out, Release{
			Version: version, Name: strings.TrimSpace(item.Name), Notes: item.Body,
			URL: item.HTMLURL, PublishedAt: item.PublishedAt, Prerelease: item.Prerelease,
			Asset: viewAsset,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].PublishedAt.After(out[j].PublishedAt) })
	return out, nil
}

func selectPackage(assets []githubAsset) *githubAsset {
	wantSuffix := fmt.Sprintf("-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "linux" {
		wantSuffix += ".tar.gz"
	} else if runtime.GOOS == "windows" {
		wantSuffix += ".zip"
	} else {
		return nil
	}
	for i := range assets {
		if strings.HasPrefix(assets[i].Name, "muxapi-") && strings.HasSuffix(assets[i].Name, wantSuffix) {
			return &assets[i]
		}
	}
	return nil
}

// Apply downloads, verifies and stages a selected release, then starts a
// helper copy of the current executable. The parent is stopped only after the
// HTTP response has been sent, so the browser receives a deterministic result.
func (s *Service) Apply(ctx context.Context, version string) (*ApplyResult, error) {
	s.mu.Lock()
	if s.updating {
		s.mu.Unlock()
		return nil, ErrInProgress
	}
	s.updating = true
	s.mu.Unlock()
	if s.restart == nil {
		s.mu.Lock()
		s.updating = false
		s.mu.Unlock()
		return nil, errors.New("更新服务未配置重启入口")
	}
	defer func() {
		s.mu.Lock()
		s.updating = false
		s.mu.Unlock()
	}()

	catalog, err := s.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	version = strings.TrimSpace(version)
	var selected *Release
	for i := range catalog.Releases {
		if sameVersion(catalog.Releases[i].Version, version) {
			selected = &catalog.Releases[i]
			break
		}
	}
	if selected == nil || selected.Asset == nil {
		return nil, errors.New("所选发行版没有当前平台的软件包")
	}

	target, err := executablePath()
	if err != nil {
		return nil, err
	}
	tempDir, err := os.MkdirTemp("", "muxapi-update-")
	if err != nil {
		return nil, fmt.Errorf("创建更新目录失败: %w", err)
	}
	packagePath := filepath.Join(tempDir, selected.Asset.Name)
	if err := s.download(ctx, selected.Asset.URL, packagePath, maxPackageBytes); err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	checksum, err := s.releaseChecksum(ctx, selected.Version, selected.Asset.Name)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	if err := verifySHA256(packagePath, checksum); err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	stagedPath, err := extractPackage(packagePath, tempDir)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	if err := makeExecutable(stagedPath); err != nil {
		os.RemoveAll(tempDir)
		return nil, fmt.Errorf("设置更新程序权限失败: %w", err)
	}
	manifestPath, err := stageManifest(tempDir, target, stagedPath, s.healthURL)
	if err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	if err := launchHelper(tempDir, manifestPath); err != nil {
		os.RemoveAll(tempDir)
		return nil, err
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		s.restart()
	}()
	return &ApplyResult{Version: selected.Version, Status: "restarting"}, nil
}

func (s *Service) releaseChecksum(ctx context.Context, version, packageName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiBase+"/releases/tags/"+urlPathEscape(version), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "MuxAPI-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("读取发行版校验文件失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("读取发行版校验文件返回 HTTP %d", resp.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("解析发行版校验文件失败: %w", err)
	}
	var checksumAsset *githubAsset
	for i := range release.Assets {
		if release.Assets[i].Name == "SHA256SUMS" {
			checksumAsset = &release.Assets[i]
			break
		}
	}
	if checksumAsset == nil {
		return "", errors.New("发行版缺少 SHA256SUMS")
	}
	content, err := s.readAsset(ctx, checksumAsset.BrowserDownloadURL, 1<<20)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && filepath.Base(strings.TrimPrefix(fields[1], "*")) == packageName {
			value := strings.ToLower(strings.TrimSpace(fields[0]))
			if len(value) == sha256.Size*2 {
				return value, nil
			}
		}
	}
	return "", fmt.Errorf("SHA256SUMS 中没有 %s", packageName)
}

func (s *Service) download(ctx context.Context, assetURL, destination string, maxBytes int64) error {
	data, err := s.readAsset(ctx, assetURL, maxBytes)
	if err != nil {
		return fmt.Errorf("下载更新包失败: %w", err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return fmt.Errorf("保存更新包失败: %w", err)
	}
	return nil
}

func (s *Service) readAsset(ctx context.Context, assetURL string, maxBytes int64) ([]byte, error) {
	parsed, err := urlParse(assetURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, errors.New("发行版下载地址不安全")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "MuxAPI-updater")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载地址返回 HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBytes {
		return nil, errors.New("更新包超过大小限制")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("更新包超过大小限制")
	}
	return data, nil
}

func extractPackage(packagePath, tempDir string) (string, error) {
	name := filepath.Base(packagePath)
	want := "muxapi-linux-amd64"
	if runtime.GOOS == "windows" {
		want = "muxapi-windows-amd64.exe"
	}
	staged := filepath.Join(tempDir, "muxapi.new")
	var reader io.Reader
	var closeReader func() error
	if strings.HasSuffix(name, ".tar.gz") {
		file, err := os.Open(packagePath)
		if err != nil {
			return "", err
		}
		gz, err := gzip.NewReader(file)
		if err != nil {
			file.Close()
			return "", err
		}
		reader = tar.NewReader(gz)
		closeReader = func() error { _ = gz.Close(); return file.Close() }
	} else if strings.HasSuffix(name, ".zip") {
		archive, err := zip.OpenReader(packagePath)
		if err != nil {
			return "", err
		}
		defer archive.Close()
		for _, item := range archive.File {
			if filepath.Base(item.Name) != want || item.FileInfo().IsDir() {
				continue
			}
			if item.UncompressedSize64 > maxBinaryBytes {
				return "", errors.New("更新程序超过大小限制")
			}
			in, err := item.Open()
			if err != nil {
				return "", err
			}
			out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
			if err == nil {
				_, err = io.CopyN(out, in, int64(item.UncompressedSize64))
				out.Close()
			}
			in.Close()
			if err != nil && !errors.Is(err, io.EOF) {
				return "", err
			}
			return staged, nil
		}
		return "", errors.New("更新包中没有可执行文件")
	} else {
		return "", errors.New("不支持的更新包格式")
	}
	defer closeReader()
	for {
		header, err := reader.(*tar.Reader).Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(header.Name) != want || header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Size < 0 || header.Size > maxBinaryBytes {
			return "", errors.New("更新程序超过大小限制")
		}
		out, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
		if err != nil {
			return "", err
		}
		_, copyErr := io.CopyN(out, reader, header.Size)
		closeErr := out.Close()
		if copyErr != nil && !errors.Is(copyErr, io.EOF) {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return staged, nil
	}
	return "", errors.New("更新包中没有可执行文件")
}

func verifySHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actual, strings.TrimSpace(expected)) {
		return fmt.Errorf("更新包校验失败：期望 %s，实际 %s", expected, actual)
	}
	return nil
}

func executablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("获取当前程序路径失败: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Abs(path)
}

type manifest struct {
	TargetPath string   `json:"target_path"`
	StagedPath string   `json:"staged_path"`
	TempDir    string   `json:"temp_dir"`
	ParentPID  int      `json:"parent_pid"`
	Args       []string `json:"args"`
	WorkingDir string   `json:"working_dir"`
	HealthURL  string   `json:"health_url"`
}

func stageManifest(tempDir, target, staged, healthURL string) (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	m := manifest{TargetPath: target, StagedPath: staged, TempDir: tempDir, ParentPID: os.Getpid(), Args: append([]string(nil), os.Args[1:]...), WorkingDir: workingDir, HealthURL: healthURL}
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	path := filepath.Join(tempDir, "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func launchHelper(tempDir, manifestPath string) error {
	target, err := executablePath()
	if err != nil {
		return err
	}
	ext := filepath.Ext(target)
	helper := filepath.Join(tempDir, "muxapi-updater"+ext)
	if err := copyFile(target, helper, 0o700); err != nil {
		return fmt.Errorf("准备更新进程失败: %w", err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	cmd := exec.Command(helper, "--muxapi-apply-update", manifestPath)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = devNull, devNull, devNull
	if err := cmd.Start(); err != nil {
		devNull.Close()
		return fmt.Errorf("启动更新进程失败: %w", err)
	}
	_ = devNull.Close()
	return nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func displayVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func sameVersion(a, b string) bool {
	left := strings.TrimPrefix(strings.TrimSpace(a), "v")
	right := strings.TrimPrefix(strings.TrimSpace(b), "v")
	if strings.EqualFold(left, right) {
		return true
	}
	if strings.Contains(left, "-") || strings.Contains(right, "-") {
		return false
	}
	leftVersion, leftOK := parseVersion(left)
	rightVersion, rightOK := parseVersion(right)
	return leftOK && rightOK && leftVersion == rightVersion
}

func newerVersion(current, candidate string) bool {
	currentRaw := strings.TrimPrefix(strings.TrimSpace(current), "v")
	candidateRaw := strings.TrimPrefix(strings.TrimSpace(candidate), "v")
	currentPrerelease := strings.Contains(currentRaw, "-")
	candidatePrerelease := strings.Contains(candidateRaw, "-")
	if candidatePrerelease && !currentPrerelease {
		return false
	}
	cur, curOK := parseVersion(current)
	next, nextOK := parseVersion(candidate)
	if !curOK || !nextOK {
		return false
	}
	for i := 0; i < 3; i++ {
		if next[i] != cur[i] {
			return next[i] > cur[i]
		}
	}
	if currentPrerelease && !candidatePrerelease {
		return true
	}
	return !sameVersion(current, candidate)
}

func parseVersion(value string) ([3]int, bool) {
	var out [3]int
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if value == "" || value == "dev" {
		return out, false
	}
	parts := strings.SplitN(value, "+", 2)[0]
	parts = strings.SplitN(parts, "-", 2)[0]
	chunks := strings.Split(parts, ".")
	if len(chunks) < 2 || len(chunks) > 3 {
		return out, false
	}
	for i := range chunks {
		n, err := strconv.Atoi(chunks[i])
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func urlPathEscape(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "/", "%2F")
}

func urlParse(value string) (*url.URL, error) { return url.Parse(value) }

// HealthURL turns the listen address into a loopback health endpoint for the
// post-restart check. Unspecified bind addresses must never be dialed directly.
func HealthURL(addr string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		if strings.HasPrefix(addr, ":") {
			port = strings.TrimPrefix(addr, ":")
		} else {
			port = "8080"
		}
		host = ""
	}
	if host == "" || host == "localhost" {
		host = "127.0.0.1"
	}
	if parsed := net.ParseIP(host); parsed != nil && parsed.IsUnspecified() {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}
