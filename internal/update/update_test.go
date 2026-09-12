package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHealthURLUsesLoopbackForWildcardBind(t *testing.T) {
	if got := HealthURL(":8081"); got != "http://127.0.0.1:8081/healthz" {
		t.Fatalf("HealthURL(:8081) = %q", got)
	}
	if got := HealthURL("0.0.0.0:9000"); got != "http://127.0.0.1:9000/healthz" {
		t.Fatalf("HealthURL(wildcard) = %q", got)
	}
}

func TestCatalogSelectsCurrentPlatformAndMarksNewer(t *testing.T) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("fixture uses the Linux amd64 release asset")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.2.0", "name": "MuxAPI v1.2.0", "html_url": "https://github.com/mirainya/MuxAPI/releases/tag/v1.2.0", "published_at": time.Now().UTC(), "assets": []map[string]any{{"name": "muxapi-v1.2.0-linux-amd64.tar.gz", "browser_download_url": "https://example.invalid/pkg", "size": 10}}},
			{"tag_name": "v1.1.0", "name": "MuxAPI v1.1.0", "html_url": "https://github.com/mirainya/MuxAPI/releases/tag/v1.1.0", "published_at": time.Now().Add(-time.Hour).UTC(), "assets": []map[string]any{{"name": "muxapi-v1.1.0-linux-amd64.tar.gz", "browser_download_url": "https://example.invalid/pkg", "size": 10}}},
		})
	}))
	defer server.Close()
	svc := New("v1.1.0", "")
	svc.apiBase = server.URL
	svc.client = server.Client()
	catalog, err := svc.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Latest != "v1.2.0" || len(catalog.Releases) != 2 {
		t.Fatalf("unexpected catalog: %+v", catalog)
	}
	if !catalog.Releases[0].Newer || catalog.Releases[1].Current {
		t.Fatalf("unexpected release flags: %+v", catalog.Releases)
	}
}

func TestExtractAndVerifyTarPackage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fixture uses the Linux package format")
	}
	dir := t.TempDir()
	packagePath := filepath.Join(dir, "muxapi-v1.2.0-linux-amd64.tar.gz")
	file, err := os.Create(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gz)
	payload := []byte("binary")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "muxapi-linux-amd64", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(mustRead(t, packagePath))
	if err := verifySHA256(packagePath, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}
	staged, err := extractPackage(packagePath, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, staged)); got != string(payload) {
		t.Fatalf("extracted payload = %q", got)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSameVersionIgnoresVPrefix(t *testing.T) {
	if !sameVersion("v1.2.3", "1.2.3") || newerVersion("v1.2.3", "v1.2.3") {
		t.Fatal("version normalization is incorrect")
	}
	if !newerVersion("v1.2.3", "v1.3.0") || newerVersion("v1.3.0", "v1.2.3") {
		t.Fatal("version comparison is incorrect")
	}
	if strings.TrimSpace(displayVersion("")) != "dev" {
		t.Fatal("empty version should display as dev")
	}
}
