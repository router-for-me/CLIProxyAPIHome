package management

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/home"
)

// The normal Anthropic client, exchange and database path must produce a usable receipt.
func TestClaudeOAuthCompletionReceipt(t *testing.T) {
	if os.Getenv("HOME_CLAUDE_RECEIPT_FIXTURE") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestClaudeOAuthCompletionReceipt$", "-test.v")
		cmd.Env = append(os.Environ(), "HOME_CLAUDE_RECEIPT_FIXTURE=1")
		output, errRun := cmd.CombinedOutput()
		t.Log(string(output))
		if errRun != nil {
			t.Fatal(errRun)
		}
		return
	}
	key, errKey := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if errKey != nil {
		t.Fatal(errKey)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(169), Subject: pkix.Name{CommonName: "OAuth fixture"},
		DNSNames: []string{"api.anthropic.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, errCert := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if errCert != nil {
		t.Fatal(errCert)
	}
	cert, errParse := x509.ParseCertificate(der)
	if errParse != nil {
		t.Fatal(errParse)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	// Force only this test's trust roots without changing the production TLS client.
	t.Setenv("GODEBUG", "x509usefallbackroots=1")
	x509.SetFallbackRoots(roots)
	requests := make(chan map[string]string, 1)
	tokenServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Host != "api.anthropic.com" || r.URL.Path != "/v1/oauth/token" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body map[string]string
		if errDecode := json.NewDecoder(r.Body).Decode(&body); errDecode != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_in":3600,"account":{"uuid":"fixture-account","email_address":"receipt@example.invalid"},"organization":{"uuid":"fixture-org","name":"Fixture"}}`)
	}))
	tokenServer.EnableHTTP2 = true
	tokenServer.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	tokenServer.StartTLS()
	defer tokenServer.Close()
	proxy, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatal(errListen)
	}
	defer proxy.Close()
	go func() {
		conn, errAccept := proxy.Accept()
		if errAccept != nil {
			return
		}
		defer conn.Close()
		greeting := make([]byte, 2)
		if _, errRead := io.ReadFull(conn, greeting); errRead != nil {
			return
		}
		methods := make([]byte, int(greeting[1]))
		if _, errRead := io.ReadFull(conn, methods); errRead != nil {
			return
		}
		_, _ = conn.Write([]byte{5, 0})
		header := make([]byte, 5)
		if _, errRead := io.ReadFull(conn, header); errRead != nil || header[3] != 3 {
			return
		}
		destination := make([]byte, int(header[4])+2)
		if _, errRead := io.ReadFull(conn, destination); errRead != nil || string(destination[:len(destination)-2]) != "api.anthropic.com" {
			return
		}
		upstream, errDial := net.Dial("tcp", tokenServer.Listener.Addr().String())
		if errDial != nil {
			return
		}
		defer upstream.Close()
		_, _ = conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0})
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(conn, upstream)
		_ = conn.Close()
		<-done
	}()
	runtime, errRuntime := home.NewRuntime(&config.Config{AuthDir: t.TempDir(), SDKConfig: config.SDKConfig{ProxyURL: "socks5://" + proxy.Addr().String()}})
	if errRuntime != nil {
		t.Fatal(errRuntime)
	}
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	h := NewHandler(repo, runtime, "127.0.0.1", 0)
	router := gin.New()
	router.GET("/oauth/auth-url", h.StartOAuthV8)
	router.GET("/oauth/status", h.GetAuthStatusV8)
	start := oauthStatusV8Request(t, router, "/oauth/auth-url?provider=claude")
	state, ok := start["state"].(string)
	if !ok || state == "" {
		t.Fatal("login did not return state")
	}
	pending, errPending := repo.GetOAuthSession(t.Context(), state)
	if errPending != nil || pending == nil {
		t.Fatal("pending login is missing")
	}
	pendingData, errData := cluster.OAuthSessionData(pending)
	if errData != nil {
		t.Fatal(errData)
	}
	// The same synchronous worker used by the public callback runs the real token exchange.
	h.processOAuthCallback("anthropic", state, "fixture-code")
	select {
	case request := <-requests:
		if request["code"] != "fixture-code" || request["state"] != state || request["code_verifier"] != pendingData["code_verifier"] || request["grant_type"] != "authorization_code" {
			t.Fatal("exchange did not preserve callback state and PKCE")
		}
	default:
		t.Fatal("normal Anthropic client did not reach the fixture")
	}
	auths, errAuths := repo.ListAuths(t.Context())
	if errAuths != nil || len(auths) != 1 {
		t.Fatal("exchange did not commit exactly one credential")
	}
	credential, _, errCredential := repo.GetAuth(t.Context(), auths[0].ID)
	if errCredential != nil || credential == nil {
		t.Fatal("saved credential is missing")
	}
	if credential.Metadata["type"] != "claude" || credential.Metadata["access_token"] != "fixture-access" || credential.Metadata["refresh_token"] != "fixture-refresh" || credential.Metadata["email"] != "receipt@example.invalid" {
		t.Fatal("normal exchange changed the stored Claude token shape")
	}
	expires, errExpires := time.Parse(time.RFC3339, credential.Metadata["expired"].(string))
	if errExpires != nil || time.Until(expires) < 59*time.Minute {
		t.Fatal("stored Claude expiry is missing or invalid")
	}
	reader := gin.New()
	reader.GET("/oauth/status", NewHandler(cluster.NewRepository(db), nil, "127.0.0.1", 0).GetAuthStatusV8)
	result := oauthStatusV8Request(t, reader, "/oauth/status?state="+url.QueryEscape(state))
	want := map[string]any{"status": "ok", "auth_id": auths[0].ID, "auth_ids": []any{auths[0].ID}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("completion receipt = %#v, want %#v", result, want)
	}
	complete, errComplete := repo.GetOAuthSession(t.Context(), state)
	if errComplete != nil || complete == nil || strings.Contains(string(complete.Data), "fixture-") || strings.Contains(string(complete.Data), "code_verifier") {
		t.Fatal("completion record retained pending secrets")
	}
	t.Log("Claude token exchange committed one credential and returned its exact ID through a fresh Home handler")
}
