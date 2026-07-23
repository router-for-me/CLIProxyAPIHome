package respserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginstore"
)

func TestServerPluginSyncPreservesMTLSFingerprint(t *testing.T) {
	runtimeHome := newSubscriptionTestRuntime(t, &blockingConfigAdapter{payload: []byte("host: \"\"\nport: 8327\n"), loadStarted: make(chan struct{})})
	conn := newSubscriptionTestConn("GET", "plugin-sync", pluginSyncServerRequest(t))
	fingerprint := peerCertificateFingerprint(conn)
	runtimeHome.SetPluginSyncNodeActive(func(_ context.Context, nodeID string, actualFingerprint string) (bool, error) {
		return nodeID == "subscription-test-node" && actualFingerprint == fingerprint, nil
	})

	serverDone := make(chan struct{})
	go func() {
		New("", runtimeHome).HandleConn(context.Background(), conn)
		close(serverDone)
	}()
	defer func() {
		if errClose := conn.Close(); errClose != nil {
			t.Errorf("close test connection: %v", errClose)
		}
		waitSubscriptionTestSignal(t, serverDone, "RESP server shutdown")
	}()

	deadline := time.Now().Add(respPipeDeadline)
	for time.Now().Before(deadline) {
		output := conn.Output()
		if strings.Contains(output, "plugin sync requires mTLS node identity") {
			t.Fatalf("plugin sync lost mTLS fingerprint: %q", output)
		}
		if strings.Contains(output, "schema_version") {
			return
		}
	}
	t.Fatalf("plugin sync did not return a plan: %q", conn.Output())
}

func pluginSyncServerRequest(t *testing.T) string {
	t.Helper()
	payload, errMarshal := json.Marshal(pluginstore.PluginSyncRequest{
		SchemaVersion: pluginstore.PluginSyncSchemaVersion,
		GOOS:          "linux",
		GOARCH:        "amd64",
	})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	return string(payload)
}
