package managementhttp

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	cpasdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	cpacoreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cpaconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	clustermanagement "github.com/router-for-me/CLIProxyAPIHome/internal/cluster/management"
)

func TestCORSMiddlewareMatchesCPAHeaders(t *testing.T) {
	engine := gin.New()
	engine.Use(corsMiddleware())
	engine.GET("/v0/management/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	optionsResp := httptest.NewRecorder()
	optionsReq := httptest.NewRequest(http.MethodOptions, "/v0/management/ping", nil)
	engine.ServeHTTP(optionsResp, optionsReq)

	if optionsResp.Code != http.StatusNoContent {
		t.Fatalf("OPTIONS status = %d, want %d", optionsResp.Code, http.StatusNoContent)
	}
	assertCORSHeaders(t, optionsResp)

	getResp := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	engine.ServeHTTP(getResp, getReq)

	if getResp.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getResp.Code, http.StatusOK)
	}
	assertCORSHeaders(t, getResp)
}

func TestClusterMTLSMiddlewareRejectsPlainHTTP(t *testing.T) {
	engine := gin.New()
	engine.Use(clusterMTLSMiddleware())
	engine.GET("/v0/cluster/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/cluster/ping", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusForbidden)
	}
}

func TestClusterMTLSMiddlewareAllowsVerifiedPeer(t *testing.T) {
	engine := gin.New()
	engine.Use(clusterMTLSMiddleware())
	engine.GET("/v0/cluster/ping", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	cert := &x509.Certificate{}
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/cluster/ping", nil)
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{cert},
		VerifiedChains:   [][]*x509.Certificate{{cert}},
	}
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
}

func TestClusterManagementBillingWriteRoutesRegistered(t *testing.T) {
	reg := newRouteRegistry()
	handler := clustermanagement.NewHandler(nil, nil, "", 0)
	registerClusterManagementRoutes(reg, handler)

	for _, route := range []RouteKey{
		{Method: http.MethodGet, Path: "/billing/overview"},
		{Method: http.MethodGet, Path: "/billing/charges"},
		{Method: http.MethodGet, Path: "/billing/balance-records"},
		{Method: http.MethodGet, Path: "/billing/model-prices"},
		{Method: http.MethodPost, Path: "/billing/balance-records/recharge"},
		{Method: http.MethodPost, Path: "/billing/balance-records/deduct"},
		{Method: http.MethodPost, Path: "/billing/model-prices"},
		{Method: http.MethodPatch, Path: "/billing/model-prices/:id"},
		{Method: http.MethodDelete, Path: "/billing/model-prices/:id"},
	} {
		if reg.routes[route] == nil {
			t.Fatalf("route %s %s was not registered", route.Method, route.Path)
		}
	}
}

func TestClusterManagementAPIKeyUsageRouteRegistered(t *testing.T) {
	reg := newRouteRegistry()
	handler := clustermanagement.NewHandler(nil, nil, "", 0)
	registerClusterManagementRoutes(reg, handler)

	route := RouteKey{Method: http.MethodGet, Path: "/api-key-usage"}
	if reg.routes[route] == nil {
		t.Fatalf("route %s %s was not registered", route.Method, route.Path)
	}
}

func TestClusterManagementUsageObservabilityRoutesRegistered(t *testing.T) {
	reg := newRouteRegistry()
	handler := clustermanagement.NewHandler(nil, nil, "", 0)
	registerClusterManagementRoutes(reg, handler)

	for _, route := range []RouteKey{
		{Method: http.MethodGet, Path: "/capabilities"},
		{Method: http.MethodGet, Path: "/usage/overview"},
		{Method: http.MethodGet, Path: "/usage/records"},
		{Method: http.MethodGet, Path: "/usage/records/:id"},
		{Method: http.MethodGet, Path: "/usage/aggregates"},
		{Method: http.MethodGet, Path: "/usage/export"},
		{Method: http.MethodGet, Path: "/usage/realtime"},
		{Method: http.MethodGet, Path: "/usage/health/providers"},
		{Method: http.MethodGet, Path: "/usage/health/credentials"},
		{Method: http.MethodGet, Path: "/request-events"},
		{Method: http.MethodGet, Path: "/request-events/export"},
		{Method: http.MethodGet, Path: "/request-events/filter-options"},
		{Method: http.MethodGet, Path: "/request-events/:id"},
		{Method: http.MethodGet, Path: "/request-logs"},
	} {
		if reg.routes[route] == nil {
			t.Fatalf("route %s %s was not registered", route.Method, route.Path)
		}
	}
}

func TestRouteRegistryRegistersStaticRoutesBeforeWildcards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	group := engine.Group("")
	reg := newRouteRegistry()
	reg.Set(http.MethodGet, "/request-events/:id", func(c *gin.Context) {
		c.String(http.StatusOK, "id:"+c.Param("id"))
	})
	reg.Set(http.MethodGet, "/request-events/export", func(c *gin.Context) {
		c.String(http.StatusOK, "export")
	})
	reg.Set(http.MethodGet, "/request-events/filter-options", func(c *gin.Context) {
		c.String(http.StatusOK, "filter-options")
	})

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Register() panic = %v", recovered)
		}
	}()
	reg.Register(group)

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/request-events/export", want: "export"},
		{path: "/request-events/filter-options", want: "filter-options"},
		{path: "/request-events/evt_1", want: "id:evt_1"},
	} {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		engine.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d", tc.path, resp.Code, http.StatusOK)
		}
		if got := resp.Body.String(); got != tc.want {
			t.Fatalf("%s body = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestClusterManagementProxyPoolRoutesRegistered(t *testing.T) {
	reg := newRouteRegistry()
	handler := clustermanagement.NewHandler(nil, nil, "", 0)
	registerClusterManagementRoutes(reg, handler)

	for _, route := range []RouteKey{
		{Method: http.MethodGet, Path: "/proxy/proxy-pools"},
		{Method: http.MethodPost, Path: "/proxy/proxy-pools"},
		{Method: http.MethodPatch, Path: "/proxy/proxy-pools/:id"},
		{Method: http.MethodDelete, Path: "/proxy/proxy-pools/:id"},
		{Method: http.MethodPost, Path: "/proxy/proxy-pools/:id/test"},
	} {
		if reg.routes[route] == nil {
			t.Fatalf("route %s %s was not registered", route.Method, route.Path)
		}
	}

	for _, route := range []RouteKey{
		{Method: http.MethodGet, Path: "/billing/proxy-pools"},
		{Method: http.MethodPost, Path: "/billing/proxy-pools"},
		{Method: http.MethodPatch, Path: "/billing/proxy-pools/:id"},
		{Method: http.MethodDelete, Path: "/billing/proxy-pools/:id"},
		{Method: http.MethodPost, Path: "/billing/proxy-pools/:id/test"},
	} {
		if reg.routes[route] != nil {
			t.Fatalf("route %s %s should not be registered", route.Method, route.Path)
		}
	}
}

func TestManagementControlPanelRoutesServeEmbeddedAssets(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)

	for _, asset := range []struct {
		name string
		body string
	}{
		{name: "index.html", body: "bridge"},
		{name: "management.html", body: "management"},
		{name: "user.html", body: "user"},
	} {
		if err := os.WriteFile(filepath.Join(staticDir, asset.name), []byte(asset.body), 0o644); err != nil {
			t.Fatalf("write %s: %v", asset.name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(staticDir, "assets", "js"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "assets", "js", "app.1234.js"), []byte("asset"), 0o644); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	engine := gin.New()
	cfg := &cpaconfig.Config{}
	registerManagementControlPanelRoutes(engine, func(assetName string) gin.HandlerFunc {
		return serveManagementControlPanel(cfg, filepath.Join(staticDir, "config.yaml"), assetName)
	}, serveManagementControlPanelAsset(cfg, filepath.Join(staticDir, "config.yaml")))

	for _, tc := range []struct {
		path string
		body string
	}{
		{path: "/", body: "bridge"},
		{path: "/index.html", body: "bridge"},
		{path: "/management.html", body: "management"},
		{path: "/user.html", body: "user"},
	} {
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		engine.ServeHTTP(resp, req)

		if resp.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want %d", tc.path, resp.Code, http.StatusOK)
		}
		if got := resp.Body.String(); got != tc.body {
			t.Fatalf("%s body = %q, want %q", tc.path, got, tc.body)
		}
		if got := resp.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("%s Cache-Control = %q, want no-cache", tc.path, got)
		}
	}

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/js/app.1234.js", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := resp.Body.String(); got != "asset" {
		t.Fatalf("asset body = %q, want asset", got)
	}
	if got := resp.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("asset Cache-Control = %q, want immutable cache", got)
	}
}

func TestManagementControlPanelRoutesDoNotCaptureManagementEndpoints(t *testing.T) {
	engine := gin.New()
	cfg := &cpaconfig.Config{}
	registerManagementControlPanelRoutes(engine, func(assetName string) gin.HandlerFunc {
		return serveManagementControlPanel(cfg, "", assetName)
	}, serveManagementControlPanelAsset(cfg, ""))
	engine.GET("/v0/management/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := resp.Body.String(); got != "pong" {
		t.Fatalf("body = %q, want pong", got)
	}
}

func TestMiddlewareSetsSupportPluginHeader(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "test-secret")
	cfg := &cpaconfig.Config{}
	handler := cpasdkapi.NewHandler(cfg, "", cpacoreauth.NewManager(nil, nil, nil))
	middleware := handler.Middleware()

	invalidResp := httptest.NewRecorder()
	invalidCtx, _ := gin.CreateTestContext(invalidResp)
	invalidReq := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	invalidReq.RemoteAddr = "127.0.0.1:12345"
	invalidReq.Header.Set("X-Management-Key", "wrong-secret")
	invalidCtx.Request = invalidReq
	middleware(invalidCtx)

	want := invalidResp.Header().Get("X-CPA-SUPPORT-PLUGIN")
	if want != "0" && want != "1" {
		t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want 0 or 1", want)
	}

	engine := gin.New()
	engine.GET("/v0/management/ping", middleware, func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/management/ping", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Management-Key", "test-secret")
	engine.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if got := resp.Header().Get("X-CPA-SUPPORT-PLUGIN"); got != want {
		t.Fatalf("X-CPA-SUPPORT-PLUGIN = %q, want %q", got, want)
	}
}

func assertCORSHeaders(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()

	if got := resp.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, PUT, PATCH, DELETE, OPTIONS" {
		t.Fatalf("Access-Control-Allow-Methods = %q, want CPA methods", got)
	}
	if got := resp.Header().Get("Access-Control-Allow-Headers"); got != "*" {
		t.Fatalf("Access-Control-Allow-Headers = %q, want *", got)
	}
	expose := resp.Header().Get("Access-Control-Expose-Headers")
	if !strings.Contains(expose, "X-CPA-SUPPORT-PLUGIN") {
		t.Fatalf("Access-Control-Expose-Headers = %q, want X-CPA-SUPPORT-PLUGIN", expose)
	}
}
