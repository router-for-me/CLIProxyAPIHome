package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadAuthFileRejectsInvalidExcludedModels(t *testing.T) {
	handler, engine, cleanup := newAPIKeyManagementRuntimeTestServer(t)
	defer cleanup()
	engine.POST("/auth-files", handler.UploadAuthFile)

	request := httptest.NewRequest(http.MethodPost, "/auth-files", strings.NewReader(`{"type":"antigravity","email":"b@example.test","excluded_models":123}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "excluded_models must be a string array") {
		t.Fatalf("body = %s, want the excluded_models validation error", body)
	}
	auths, errList := handler.repo.ListAuths(t.Context())
	if errList != nil {
		t.Fatalf("ListAuths() error = %v", errList)
	}
	if len(auths) != 0 {
		t.Fatalf("ListAuths() = %d auths, want none after a rejected upload", len(auths))
	}
}
