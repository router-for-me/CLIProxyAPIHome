package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestCreateClientCertificateAcceptsOptionalNodeName(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	now := time.Now().UTC()
	if errCreate := db.Create(&cluster.ClusterNodeRecord{IP: "home", Port: 8327, IsMaster: true, StartedAt: now, LastSeenAt: now}).Error; errCreate != nil {
		t.Fatalf("create Home record: %v", errCreate)
	}
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "home", 8327)
	engine := gin.New()
	engine.POST("/certificates/clients", handler.CreateClientCertificate)

	unnamedResponse := httptest.NewRecorder()
	engine.ServeHTTP(unnamedResponse, httptest.NewRequest(http.MethodPost, "/certificates/clients", nil))
	if unnamedResponse.Code != http.StatusOK {
		t.Fatalf("unnamed status = %d, body = %s", unnamedResponse.Code, unnamedResponse.Body.String())
	}
	var unnamedBody struct {
		NodeName *string `json:"node_name"`
	}
	if errDecode := json.Unmarshal(unnamedResponse.Body.Bytes(), &unnamedBody); errDecode != nil {
		t.Fatalf("decode unnamed response: %v", errDecode)
	}
	if unnamedBody.NodeName != nil {
		t.Fatalf("unnamed node_name = %v, want nil", *unnamedBody.NodeName)
	}

	invalidUTF8Body := append([]byte(`{"node_name":"`), 0xff, '"', '}')
	invalidUTF8Response := httptest.NewRecorder()
	engine.ServeHTTP(invalidUTF8Response, httptest.NewRequest(http.MethodPost, "/certificates/clients", bytes.NewReader(invalidUTF8Body)))
	if invalidUTF8Response.Code != http.StatusBadRequest {
		t.Fatalf("invalid UTF-8 status = %d, body = %s", invalidUTF8Response.Code, invalidUTF8Response.Body.String())
	}
	var invalidUTF8Error struct {
		Error string `json:"error"`
	}
	if errDecode := json.Unmarshal(invalidUTF8Response.Body.Bytes(), &invalidUTF8Error); errDecode != nil {
		t.Fatalf("decode invalid UTF-8 response: %v", errDecode)
	}
	if invalidUTF8Error.Error != "invalid_request" {
		t.Fatalf("invalid UTF-8 error = %q, want invalid_request", invalidUTF8Error.Error)
	}
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "null with vertical tab", body: append([]byte("null"), 0x0b)},
		{name: "object with vertical tab", body: append([]byte(`{"node_name":"x"}`), 0x0b)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/certificates/clients", bytes.NewReader(test.body)))
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s, want 400", response.Code, response.Body.String())
			}
		})
	}

	nullResponse := httptest.NewRecorder()
	engine.ServeHTTP(nullResponse, httptest.NewRequest(http.MethodPost, "/certificates/clients", strings.NewReader("null")))
	if nullResponse.Code != http.StatusOK {
		t.Fatalf("null-body status = %d, body = %s", nullResponse.Code, nullResponse.Body.String())
	}
	var nullBody struct {
		NodeName *string `json:"node_name"`
	}
	if errDecode := json.Unmarshal(nullResponse.Body.Bytes(), &nullBody); errDecode != nil {
		t.Fatalf("decode null response: %v", errDecode)
	}
	if nullBody.NodeName != nil {
		t.Fatalf("null-body node_name = %v, want nil", *nullBody.NodeName)
	}

	namedResponse := httptest.NewRecorder()
	engine.ServeHTTP(namedResponse, httptest.NewRequest(http.MethodPost, "/certificates/clients", strings.NewReader(`{"node_name":"  primary-cpa  "}`)))
	if namedResponse.Code != http.StatusOK {
		t.Fatalf("named status = %d, body = %s", namedResponse.Code, namedResponse.Body.String())
	}
	var namedBody struct {
		ID       string  `json:"id"`
		NodeName *string `json:"node_name"`
	}
	if errDecode := json.Unmarshal(namedResponse.Body.Bytes(), &namedBody); errDecode != nil {
		t.Fatalf("decode named response: %v", errDecode)
	}
	if namedBody.ID == "" || namedBody.NodeName == nil || *namedBody.NodeName != "primary-cpa" {
		t.Fatalf("named response = %+v, want primary-cpa", namedBody)
	}
}

func TestCPANodeNameRejectsUnpairedJSONSurrogates(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	if errCreate := db.Create(&cluster.CertificateRecord{ID: "cpa-a", IsClient: true}).Error; errCreate != nil {
		t.Fatalf("create certificate: %v", errCreate)
	}
	handler := NewHandler(cluster.NewRepository(db), nil, "home", 8327)
	engine := gin.New()
	engine.POST("/certificates/clients", handler.CreateClientCertificate)
	engine.PATCH("/nodes/:node_id", handler.UpdateCPANodeName)

	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantName   string
	}{
		{name: "post high surrogate", method: http.MethodPost, path: "/certificates/clients", body: `{"node_name":"\ud800"}`, wantStatus: http.StatusBadRequest},
		{name: "post low surrogate", method: http.MethodPost, path: "/certificates/clients", body: `{"node_name":"\udc00"}`, wantStatus: http.StatusBadRequest},
		{name: "patch high surrogate", method: http.MethodPatch, path: "/nodes/cpa-a", body: `{"node_name":"\ud800"}`, wantStatus: http.StatusBadRequest},
		{name: "patch low surrogate", method: http.MethodPatch, path: "/nodes/cpa-a", body: `{"node_name":"\udc00"}`, wantStatus: http.StatusBadRequest},
		{name: "patch paired surrogate", method: http.MethodPatch, path: "/nodes/cpa-a", body: `{"node_name":"\ud83d\ude80"}`, wantStatus: http.StatusOK, wantName: "🚀"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, body = %s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			if test.wantStatus == http.StatusBadRequest {
				var errorBody struct {
					Error string `json:"error"`
				}
				if errDecode := json.Unmarshal(response.Body.Bytes(), &errorBody); errDecode != nil {
					t.Fatalf("decode error response: %v", errDecode)
				}
				if errorBody.Error != "invalid_request" {
					t.Fatalf("error = %q, want invalid_request", errorBody.Error)
				}
				return
			}
			var body struct {
				NodeName *string `json:"node_name"`
			}
			if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			if body.NodeName == nil || *body.NodeName != test.wantName {
				t.Fatalf("node_name = %v, want %q", body.NodeName, test.wantName)
			}
		})
	}
}

func TestUpdateCPANodeNameAPI(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	if errCreate := db.Create(&cluster.CertificateRecord{ID: "cpa-a", IsClient: true}).Error; errCreate != nil {
		t.Fatalf("create certificate: %v", errCreate)
	}
	if errCreate := db.Create(&cluster.CertificateRecord{ID: "pending", EnrollmentSecretHash: "pending"}).Error; errCreate != nil {
		t.Fatalf("create pending certificate: %v", errCreate)
	}
	handler := NewHandler(cluster.NewRepository(db), nil, "home", 8327)
	engine := gin.New()
	engine.PATCH("/nodes/:node_id", handler.UpdateCPANodeName)

	patch := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/nodes/cpa-a", strings.NewReader(body)))
		return response
	}
	response := patch(`{"node_name":"  primary-cpa  "}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"node_name":"primary-cpa"`) {
		t.Fatalf("set response = %d %s", response.Code, response.Body.String())
	}
	response = patch(`{"node_name":null}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"node_name":null`) {
		t.Fatalf("clear response = %d %s", response.Code, response.Body.String())
	}
	response = patch(`{"node_name":"  "}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"node_name":null`) {
		t.Fatalf("blank clear response = %d %s", response.Code, response.Body.String())
	}
	response = patch(`{}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing field status = %d, want 400", response.Code)
	}
	response = patch(`{"name":"wrong-field"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", response.Code)
	}
	response = patch(`{"node_name":"primary-cpa"}{"node_name":"second"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d, want 400", response.Code)
	}
	response = patch(strings.Repeat("x", int(cpaNodeNameMaxRequestBodySize)+1))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", response.Code)
	}
	response = patch(`{"node_name":"bad\nname"}`)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid name status = %d, want 422", response.Code)
	}
	response = func() *httptest.ResponseRecorder {
		result := httptest.NewRecorder()
		engine.ServeHTTP(result, httptest.NewRequest(http.MethodPatch, "/nodes/pending", strings.NewReader(`{"node_name":"pending"}`)))
		return result
	}()
	if response.Code != http.StatusNotFound {
		t.Fatalf("pending certificate status = %d, want 404", response.Code)
	}

	notFound := httptest.NewRecorder()
	engine.ServeHTTP(notFound, httptest.NewRequest(http.MethodPatch, "/nodes/missing", strings.NewReader(`{"node_name":"missing"}`)))
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("missing node status = %d, want 404", notFound.Code)
	}
}

func TestNodeNameAppearsInNodesAndTopology(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	now := time.Now().UTC()
	home := cluster.ClusterNodeRecord{IP: "home", Port: 8327, IsMaster: true, StartedAt: now, LastSeenAt: now}
	if errCreate := db.Create(&home).Error; errCreate != nil {
		t.Fatalf("create Home record: %v", errCreate)
	}
	seedActiveManagementCPA(t, db, home, "fp-active", "cpa-active", "10.0.0.1", now, now)
	seedActiveManagementCPA(t, db, home, "fp-unnamed", "cpa-unnamed", "10.0.0.3", now.Add(time.Second), now)
	if errCreate := db.Create(&cluster.CPANodeRecord{
		HomeIP:                 "old-home",
		HomePort:               8327,
		HomeStartedAt:          now.Add(-time.Minute),
		NodeKey:                "fingerprint:fp-draining",
		NodeID:                 "cpa-draining",
		ClientIP:               "10.0.0.2",
		ClientCount:            1,
		CertificateFingerprint: "fp-draining",
		ConnectedAt:            now.Add(-time.Minute),
		LastSeenAt:             now,
	}).Error; errCreate != nil {
		t.Fatalf("create draining snapshot: %v", errCreate)
	}
	if errCreate := db.Create(&[]cluster.CPANodeMetadataRecord{
		{NodeID: "cpa-active", NodeName: "active-cpa"},
		{NodeID: "cpa-draining", NodeName: "draining-cpa"},
	}).Error; errCreate != nil {
		t.Fatalf("create node metadata: %v", errCreate)
	}
	handler := NewHandler(repo, nil, "home", 8327)
	engine := gin.New()
	engine.GET("/nodes", handler.ListNodes)
	engine.GET("/topology", handler.GetTopology)

	nodesResponse := httptest.NewRecorder()
	engine.ServeHTTP(nodesResponse, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	if nodesResponse.Code != http.StatusOK {
		t.Fatalf("nodes status = %d, body = %s", nodesResponse.Code, nodesResponse.Body.String())
	}
	var nodesBody struct {
		Nodes []struct {
			NodeID   string  `json:"node_id"`
			NodeName *string `json:"node_name"`
		} `json:"nodes"`
	}
	if errDecode := json.Unmarshal(nodesResponse.Body.Bytes(), &nodesBody); errDecode != nil {
		t.Fatalf("decode nodes response: %v", errDecode)
	}
	if len(nodesBody.Nodes) != 2 {
		t.Fatalf("nodes = %+v, want active and unnamed CPA", nodesBody.Nodes)
	}
	if active := findNodeNameTestNode(nodesBody.Nodes, "cpa-active"); active.NodeName == nil || *active.NodeName != "active-cpa" {
		t.Fatalf("active node = %+v, want active-cpa", active)
	}
	if unnamed := findNodeNameTestNode(nodesBody.Nodes, "cpa-unnamed"); unnamed.NodeName != nil {
		t.Fatalf("unnamed node_name = %v, want nil", *unnamed.NodeName)
	}

	topologyResponse := httptest.NewRecorder()
	engine.ServeHTTP(topologyResponse, httptest.NewRequest(http.MethodGet, "/topology", nil))
	if topologyResponse.Code != http.StatusOK {
		t.Fatalf("topology status = %d, body = %s", topologyResponse.Code, topologyResponse.Body.String())
	}
	var topologyBody struct {
		CPAs []struct {
			NodeID   string  `json:"node_id"`
			NodeName *string `json:"node_name"`
			State    string  `json:"state"`
		} `json:"cpas"`
	}
	if errDecode := json.Unmarshal(topologyResponse.Body.Bytes(), &topologyBody); errDecode != nil {
		t.Fatalf("decode topology response: %v", errDecode)
	}
	if node := findNodeNameTestCPA(topologyBody.CPAs, "cpa-active"); node.NodeName == nil || *node.NodeName != "active-cpa" || node.State != "active" {
		t.Fatalf("active topology CPA = %+v", node)
	}
	if node := findNodeNameTestCPA(topologyBody.CPAs, "cpa-draining"); node.NodeName == nil || *node.NodeName != "draining-cpa" || node.State != "draining" {
		t.Fatalf("draining topology CPA = %+v", node)
	}
	if node := findNodeNameTestCPA(topologyBody.CPAs, "cpa-unnamed"); node.NodeName != nil {
		t.Fatalf("unnamed topology CPA node_name = %v, want nil", *node.NodeName)
	}
}

func findNodeNameTestNode(items []struct {
	NodeID   string  `json:"node_id"`
	NodeName *string `json:"node_name"`
}, nodeID string) struct {
	NodeID   string  `json:"node_id"`
	NodeName *string `json:"node_name"`
} {
	for _, item := range items {
		if item.NodeID == nodeID {
			return item
		}
	}
	return struct {
		NodeID   string  `json:"node_id"`
		NodeName *string `json:"node_name"`
	}{}
}

func findNodeNameTestCPA(items []struct {
	NodeID   string  `json:"node_id"`
	NodeName *string `json:"node_name"`
	State    string  `json:"state"`
}, nodeID string) struct {
	NodeID   string  `json:"node_id"`
	NodeName *string `json:"node_name"`
	State    string  `json:"state"`
} {
	for _, item := range items {
		if item.NodeID == nodeID {
			return item
		}
	}
	return struct {
		NodeID   string  `json:"node_id"`
		NodeName *string `json:"node_name"`
		State    string  `json:"state"`
	}{}
}
