package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

func TestChannelGroupCredentialReads(t *testing.T) {
	h, db, closeRepo := newInFlightManagementTestHandler(t)
	t.Cleanup(closeRepo)
	auths := []cluster.AuthRecord{
		{UUID: "oauth", ID: "oauth", Index: "oauth", Label: "person@example.test", Provider: "codex"},
		{UUID: "apikey", ID: "apikey", Index: "apikey", CompatName: "Team", Provider: "compat", Status: "disabled", Unavailable: true, APIKeyHash: "private-hash"},
		{UUID: "cool", ID: "cool", Index: "cool", Unavailable: true},
		{UUID: "removed", ID: "removed", Index: "removed", Disabled: true},
		{UUID: "legacy", ID: "alias", Index: "alias", Label: "Do not resolve"},
	}
	for i := range auths {
		auths[i].AuthJSON = cluster.JSONB(`{"token":"private-token"}`)
	}
	if err := db.Create(&auths).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Delete(&auths[3]).Error; err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"oauth", "apikey", "cool", "removed", "legacy", "alias", "missing", "oauth"} {
		if err := db.Create(&cluster.ChannelGroupDetailRecord{ChannelGroupID: 1, AuthID: id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	authQueries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("test:credential_projection", func(tx *gorm.DB) {
		if tx.Statement.Table != "auth" {
			return
		}
		authQueries++
		fields := strings.Join(tx.Statement.Selects, ",")
		if fields == "" || strings.Contains(fields, "*") || strings.Contains(fields, "auth_json") || strings.Contains(fields, "api_key_hash") {
			t.Errorf("unexpected credential projection: %q", fields)
		}
	}); err != nil {
		t.Fatal(err)
	}
	routes := gin.New()
	routes.GET("/channel-group-details", h.ListChannelGroupDetails)
	routes.GET("/channel-group-details/:id", h.GetChannelGroupDetail)
	routes.GET("/channel-group-credential-candidates", h.ListChannelGroupCredentialCandidates)
	get := func(path string) map[string]json.RawMessage {
		t.Helper()
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
		for _, private := range []string{"private-token", "private-hash", "person@example.test", "Do not resolve"} {
			if strings.Contains(rec.Body.String(), private) {
				t.Fatalf("GET %s exposed %s", path, private)
			}
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	var basic, enriched []map[string]json.RawMessage
	if err := json.Unmarshal(get("/channel-group-details")["channel_group_details"], &basic); err != nil {
		t.Fatal(err)
	}
	if len(basic) != 8 || authQueries != 0 {
		t.Fatalf("basic rows=%d credential queries=%d", len(basic), authQueries)
	}
	if err := json.Unmarshal(get("/channel-group-details?include=credential")["channel_group_details"], &enriched); err != nil {
		t.Fatal(err)
	}
	if len(enriched) != len(basic) || authQueries != 1 {
		t.Fatalf("enriched rows=%d credential queries=%d", len(enriched), authQueries)
	}
	expected := map[string]cluster.ChannelGroupCredential{
		"oauth":   {AuthID: "oauth", DisplayName: "pe***@example.test", Provider: "codex"},
		"apikey":  {AuthID: "apikey", DisplayName: "Team", Provider: "compat", State: "disabled"},
		"cool":    {AuthID: "cool", DisplayName: "cool", State: "unavailable"},
		"removed": {AuthID: "removed", DisplayName: "removed", State: "deleted"},
	}
	for i, row := range enriched {
		var authID string
		if err := json.Unmarshal(row["auth_id"], &authID); err != nil {
			t.Fatal(err)
		}
		if want, ok := expected[authID]; ok {
			var got cluster.ChannelGroupCredential
			if err := json.Unmarshal(row["credential"], &got); err != nil || got != want {
				t.Fatalf("%s metadata=%+v err=%v", authID, got, err)
			}
		} else if string(row["credential"]) != "null" {
			t.Fatalf("%s resolved to %s", authID, row["credential"])
		}
		delete(row, "credential")
		if !reflect.DeepEqual(row, basic[i]) {
			t.Fatalf("binding row %d changed", i)
		}
	}
	var single map[string]json.RawMessage
	if err := json.Unmarshal(get("/channel-group-details/4?include=credential")["channel_group_detail"], &single); err != nil {
		t.Fatal(err)
	}
	if string(single["deleted_at"]) != "null" || !strings.Contains(string(single["credential"]), `"state":"deleted"`) {
		t.Fatalf("single row: %v", single)
	}
	var candidates []cluster.ChannelGroupCredential
	if err := json.Unmarshal(get("/channel-group-credential-candidates")["credential_candidates"], &candidates); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(candidates, []cluster.ChannelGroupCredential{expected["apikey"], expected["cool"], expected["oauth"]}) {
		t.Errorf("candidates=%+v", candidates)
	}
	// A failed identity lookup must preserve every binding for inspection and removal.
	if err := db.Migrator().DropTable(&cluster.AuthRecord{}); err != nil {
		t.Fatal(err)
	}
	partial := get("/channel-group-details?include=credential")
	if string(partial["credential_metadata_unavailable"]) != "true" || !reflect.DeepEqual(partial["channel_group_details"], get("/channel-group-details")["channel_group_details"]) {
		t.Fatalf("unexpected partial response: %s", partial["channel_group_details"])
	}
}
