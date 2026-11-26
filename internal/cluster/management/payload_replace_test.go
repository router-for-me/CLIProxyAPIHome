package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gopkg.in/yaml.v3"
)

const payloadReplaceRulesFixture = `[
  {
    "models": [{"name":"*","protocol":"claude","from-protocol":"openai","headers":{"X-Tier":"prod-*"},"match":[{"stage":"ready"}],"not-match":[{"blocked":true}],"exist":["messages"],"not-exist":["disabled"]}],
    "target": {"role":"user","positions":[1,2,2]},
    "replacements": [{"you are hero":"you are fool"},{"you are fool":"you are friend"},{"remove":""}]
  },
  {
    "models": [{"name":"gemini-*","protocol":"gemini"}],
    "target": {"role":"user","all":true},
    "replacements": [{"1":"false"}]
  }
]`

func TestPayloadReplaceV8DatabaseRoundTrip(t *testing.T) {
	db, cleanup := openManagementLogTestDB(t)
	defer cleanup()
	repo := cluster.NewRepository(db)
	handler := NewHandler(repo, nil, "127.0.0.1", 0)
	router := gin.New()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		router.Handle(method, "/v8/management/config/*path", handler.ConfigV8)
	}
	router.GET("/v8/management/config", handler.ConfigV8)
	router.GET("/v8/management/config.yaml", handler.ConfigV8)
	router.PUT("/v8/management/config.yaml", handler.ConfigV8)
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if strings.HasSuffix(path, ".yaml") {
			req.Header.Set("Content-Type", "application/yaml")
		} else {
			req.Header.Set("Content-Type", "application/json")
		}
		router.ServeHTTP(recorder, req)
		if recorder.Code != status {
			t.Fatalf("%s %s: status=%d, want %d: %s", method, path, recorder.Code, status, recorder.Body.String())
		}
		return recorder.Body.Bytes()
	}
	var expected any
	if errDecode := json.Unmarshal([]byte(payloadReplaceRulesFixture), &expected); errDecode != nil {
		t.Fatal(errDecode)
	}
	assertRules := func(value any) {
		t.Helper()
		encoded, errEncode := json.Marshal(value)
		if errEncode != nil {
			t.Fatal(errEncode)
		}
		var normalized any
		if errDecode := json.Unmarshal(encoded, &normalized); errDecode != nil {
			t.Fatal(errDecode)
		}
		if !reflect.DeepEqual(normalized, expected) {
			t.Fatalf("replacement order, selection, or matchers changed: %s", encoded)
		}
	}
	const base = "/v8/management/config/requests/payload"
	request(http.MethodPut, base, `{"override":[{"models":[{"name":"*"}],"params":{"max_tokens":100}}],"replace":`+payloadReplaceRulesFixture+`,"filter":[]}`, http.StatusOK)
	read := request(http.MethodGet, base+"/replace", "", http.StatusOK)
	var actual any
	if errDecode := json.Unmarshal(read, &actual); errDecode != nil {
		t.Fatal(errDecode)
	}
	assertRules(actual)
	request(http.MethodPatch, base, `{"filter":[{"models":[{"name":"*"}],"params":["metadata.debug"]}]}`, http.StatusOK)
	var payload map[string]any
	if errDecode := json.Unmarshal(request(http.MethodGet, base, "", http.StatusOK), &payload); errDecode != nil {
		t.Fatal(errDecode)
	}
	assertRules(payload["replace"])
	if len(payload["override"].([]any)) != 1 || len(payload["filter"].([]any)) != 1 {
		t.Fatal("sibling payload sections were lost")
	}

	// Runtime distribution must preserve the exact business rules, not SDK defaults.
	cfg, wire, errLoad := repo.LoadConfigAsRuntimeConfig(context.Background())
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg == nil {
		t.Fatal("missing Home runtime config")
	}
	var distributed map[string]any
	if errDecode := yaml.Unmarshal(wire, &distributed); errDecode != nil {
		t.Fatal(errDecode)
	}
	assertRules(distributed["payload"].(map[string]any)["replace"])

	// YAML export/import is an exchange path; it must keep ordered mappings intact.
	exported := request(http.MethodGet, "/v8/management/config.yaml", "", http.StatusOK)
	request(http.MethodPut, "/v8/management/config.yaml", string(exported), http.StatusOK)
	read = request(http.MethodGet, base+"/replace", "", http.StatusOK)
	if errDecode := json.Unmarshal(read, &actual); errDecode != nil {
		t.Fatal(errDecode)
	}
	assertRules(actual)

	before, errBefore := repo.LoadConfigSnapshot(context.Background())
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	for _, invalid := range []string{
		`[{"models":[{"name":"*"}],"target":{"role":"user","positions":[0]},"replacements":[{"a":"b"}]}]`,
		`[{"models":[{"name":"*"}],"target":{"role":"user","positions":[1],"all":true},"replacements":[{"a":"b"}]}]`,
		`[{"models":[{"name":"*"}],"target":{"role":"user","all":true},"replacements":[{"":"b"}]}]`,
		`[{"models":[{"name":"*"}],"target":{"role":"user","all":true},"replacements":[{"a":null}]}]`,
		`[{"models":[{"name":"*"}],"target":{"role":"user","all":true},"replacements":[{"a":"b","c":"d"}]}]`,
		`[{"models":[{"name":"*"}],"target":{"role":"user","all":true},"replacements":[{"a":"b"}],"typo":true}]`,
	} {
		request(http.MethodPut, base+"/replace", invalid, http.StatusBadRequest)
	}
	after, errAfter := repo.LoadConfigSnapshot(context.Background())
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("rejected replacement write modified the database")
	}

	// Lists replace whole: target changes must not merge old positions or mappings.
	updated := `[{"models":[{"name":"*"}],"target":{"role":"user","all":true},"replacements":[{"new-source":""}]}]`
	request(http.MethodPatch, base, `{"replace":`+updated+`}`, http.StatusOK)
	read = request(http.MethodGet, base+"/replace", "", http.StatusOK)
	if errDecode := json.Unmarshal([]byte(updated), &expected); errDecode != nil {
		t.Fatal(errDecode)
	}
	if errDecode := json.Unmarshal(read, &actual); errDecode != nil {
		t.Fatal(errDecode)
	}
	assertRules(actual)
	request(http.MethodDelete, base+"/replace", "", http.StatusOK)
	request(http.MethodGet, base+"/replace", "", http.StatusNotFound)
	request(http.MethodGet, base+"/override", "", http.StatusOK)
	request(http.MethodPut, base+"/replace", `[]`, http.StatusOK)
	if read := request(http.MethodGet, base+"/replace", "", http.StatusOK); string(read) != "[]" {
		t.Fatalf("empty replacement list changed: %s", read)
	}
}
