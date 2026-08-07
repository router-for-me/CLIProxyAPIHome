package managementhttp

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// newAssetTestEngine wires the control panel routes against a temporary static
// directory, mirroring how MANAGEMENT_STATIC_PATH overrides the embedded FS.
func newAssetTestEngine(t *testing.T, files map[string][]byte) *gin.Engine {
	t.Helper()

	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	for name, body := range files {
		full := filepath.Join(staticDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	engine := gin.New()
	cfg := &cpaconfig.Config{}
	registerManagementControlPanelRoutes(engine, func(assetName string) gin.HandlerFunc {
		return serveManagementControlPanel(cfg, filepath.Join(staticDir, "config.yaml"), assetName)
	}, serveManagementControlPanelAsset(cfg, filepath.Join(staticDir, "config.yaml")))

	return engine
}

func getAsset(t *testing.T, engine *gin.Engine, path string, acceptEncoding string) *httptest.ResponseRecorder {
	t.Helper()
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if acceptEncoding != "" {
		req.Header.Set("Accept-Encoding", acceptEncoding)
	}
	engine.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("%s status = %d, want %d", path, resp.Code, http.StatusOK)
	}
	return resp
}

func TestControlPanelAssetsAreGzippedWhenAccepted(t *testing.T) {
	// Highly compressible and comfortably over the size floor.
	body := []byte(strings.Repeat("export const compressible = true;\n", 400))
	engine := newAssetTestEngine(t, map[string][]byte{"assets/js/app.1234.js": body})

	resp := getAsset(t, engine, "/assets/js/app.1234.js", "gzip, deflate, br")

	if got := resp.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("Vary = %q, want Accept-Encoding", got)
	}
	if got := resp.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatalf("Content-Type = %q, want a javascript type", got)
	}
	if got := resp.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control = %q, want the immutable policy", got)
	}

	if resp.Body.Len() >= len(body) {
		t.Fatalf("compressed size %d did not shrink the %d byte asset", resp.Body.Len(), len(body))
	}

	reader, errReader := gzip.NewReader(resp.Body)
	if errReader != nil {
		t.Fatalf("gzip.NewReader: %v", errReader)
	}
	decoded, errRead := io.ReadAll(reader)
	if errRead != nil {
		t.Fatalf("read gzip body: %v", errRead)
	}
	if string(decoded) != string(body) {
		t.Fatalf("decompressed body did not round-trip (%d bytes vs %d)", len(decoded), len(body))
	}
}

func TestControlPanelAssetsServeIdentityWhenGzipNotAccepted(t *testing.T) {
	body := []byte(strings.Repeat("export const compressible = true;\n", 400))
	engine := newAssetTestEngine(t, map[string][]byte{"assets/js/app.1234.js": body})

	for _, acceptEncoding := range []string{"", "identity", "gzip;q=0"} {
		resp := getAsset(t, engine, "/assets/js/app.1234.js", acceptEncoding)

		if got := resp.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("Accept-Encoding %q: Content-Encoding = %q, want none", acceptEncoding, got)
		}
		if got := resp.Header().Get("Vary"); got != "Accept-Encoding" {
			t.Fatalf("Accept-Encoding %q: Vary = %q, want Accept-Encoding", acceptEncoding, got)
		}
		if resp.Body.String() != string(body) {
			t.Fatalf("Accept-Encoding %q: body was not served verbatim", acceptEncoding)
		}
	}
}

func TestControlPanelAssetsSkipAlreadyCompressedAndTinyPayloads(t *testing.T) {
	// A woff2 is already compressed; a short script is below the size floor.
	font := []byte(strings.Repeat("f", 40_000))
	tiny := []byte("export const x = 1;\n")
	engine := newAssetTestEngine(t, map[string][]byte{
		"assets/font/inter.9999.woff2": font,
		"assets/js/tiny.1234.js":       tiny,
	})

	for _, tc := range []struct {
		path string
		body []byte
		why  string
	}{
		{path: "/assets/font/inter.9999.woff2", body: font, why: "already-compressed type"},
		{path: "/assets/js/tiny.1234.js", body: tiny, why: "below the size floor"},
	} {
		resp := getAsset(t, engine, tc.path, "gzip")
		if got := resp.Header().Get("Content-Encoding"); got != "" {
			t.Fatalf("%s (%s): Content-Encoding = %q, want none", tc.path, tc.why, got)
		}
		if resp.Body.String() != string(tc.body) {
			t.Fatalf("%s (%s): body was not served verbatim", tc.path, tc.why)
		}
	}
}

func TestControlPanelHTMLIsGzippedButStaysRevalidated(t *testing.T) {
	body := []byte("<!doctype html><html><body>" + strings.Repeat("<div>panel</div>", 200) + "</body></html>")
	engine := newAssetTestEngine(t, map[string][]byte{"management.html": body})

	resp := getAsset(t, engine, "/management.html", "gzip")

	if got := resp.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	// The HTML entry must keep revalidating so a new build is picked up even
	// though its hashed assets are cached forever.
	if got := resp.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
}

func TestControlPanelAssetCompressionIsCachedAcrossRequests(t *testing.T) {
	body := []byte(strings.Repeat("export const compressible = true;\n", 400))
	engine := newAssetTestEngine(t, map[string][]byte{"assets/js/app.1234.js": body})

	first := getAsset(t, engine, "/assets/js/app.1234.js", "gzip")
	second := getAsset(t, engine, "/assets/js/app.1234.js", "gzip")

	if first.Body.String() != second.Body.String() {
		t.Fatal("cached compression returned a different payload on the second request")
	}
	if got := second.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("second request Content-Encoding = %q, want gzip", got)
	}
	if got := second.Header().Get("Content-Length"); got != "" {
		if got != first.Header().Get("Content-Length") {
			t.Fatalf("Content-Length changed between requests: %q vs %q", first.Header().Get("Content-Length"), got)
		}
	}
}

func TestClientAcceptsGzip(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{header: "", want: false},
		{header: "gzip", want: true},
		{header: "gzip, deflate, br", want: true},
		{header: "br, gzip;q=0.8", want: true},
		{header: "gzip;q=0", want: false},
		{header: "identity", want: false},
		{header: "deflate", want: false},
		{header: "GZIP", want: true},
	} {
		req := httptest.NewRequest(http.MethodGet, "/assets/js/app.js", nil)
		if tc.header != "" {
			req.Header.Set("Accept-Encoding", tc.header)
		}
		if got := clientAcceptsGzip(req); got != tc.want {
			t.Fatalf("clientAcceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
