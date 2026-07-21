package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

type fakeQuotaRecollectTrigger struct {
	accepted      int
	err           error
	credentialIDs map[string]struct{}
	providers     map[string]struct{}
	calls         int
}

func (f *fakeQuotaRecollectTrigger) TriggerCollection(_ context.Context, credentialIDs map[string]struct{}, providers map[string]struct{}) (int, error) {
	f.calls++
	f.credentialIDs = credentialIDs
	f.providers = providers
	return f.accepted, f.err
}

func TestCollectQuotaAcceptedWithFilters(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	trigger := &fakeQuotaRecollectTrigger{accepted: 2}
	handler.SetQuotaRecollectTrigger(trigger)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/quota/collect", handler.CollectQuota)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/quota/collect", strings.NewReader(`{"credential_ids":["auth-a"," auth-b "],"providers":["xai"]}`))
	request.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted {
		t.Fatalf("collect status = %d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if errDecode := json.Unmarshal(response.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode collect response: %v", errDecode)
	}
	if payload["accepted"] != float64(2) || payload["running"] != true {
		t.Fatalf("unexpected collect payload: %#v", payload)
	}
	if trigger.calls != 1 {
		t.Fatalf("trigger calls = %d, want 1", trigger.calls)
	}
	if len(trigger.credentialIDs) != 2 || len(trigger.providers) != 1 {
		t.Fatalf("unexpected trigger filters: ids=%v providers=%v", trigger.credentialIDs, trigger.providers)
	}
	if _, ok := trigger.credentialIDs["auth-b"]; !ok {
		t.Fatalf("credential id whitespace was not trimmed: %v", trigger.credentialIDs)
	}
}

func TestCollectQuotaEmptyBodyCollectsAll(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	trigger := &fakeQuotaRecollectTrigger{accepted: 5}
	handler.SetQuotaRecollectTrigger(trigger)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/quota/collect", handler.CollectQuota)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/quota/collect", nil))

	if response.Code != http.StatusAccepted {
		t.Fatalf("collect status = %d body=%s", response.Code, response.Body.String())
	}
	if trigger.calls != 1 || len(trigger.credentialIDs) != 0 || len(trigger.providers) != 0 {
		t.Fatalf("unexpected trigger invocation: calls=%d ids=%v providers=%v", trigger.calls, trigger.credentialIDs, trigger.providers)
	}
}

func TestCollectQuotaRejectsUnsupportedProvider(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	trigger := &fakeQuotaRecollectTrigger{}
	handler.SetQuotaRecollectTrigger(trigger)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/quota/collect", handler.CollectQuota)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/quota/collect", strings.NewReader(`{"providers":["gemini"]}`)))

	if response.Code != http.StatusBadRequest {
		t.Fatalf("collect status = %d body=%s", response.Code, response.Body.String())
	}
	if trigger.calls != 0 {
		t.Fatalf("trigger should not be called for invalid providers")
	}
}

func TestCollectQuotaNormalizesGrokProvider(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()
	trigger := &fakeQuotaRecollectTrigger{accepted: 1}
	handler.SetQuotaRecollectTrigger(trigger)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/quota/collect", handler.CollectQuota)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/quota/collect", strings.NewReader(`{"providers":["grok"]}`)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("collect status = %d body=%s", response.Code, response.Body.String())
	}
	if _, ok := trigger.providers["xai"]; !ok || len(trigger.providers) != 1 {
		t.Fatalf("normalized providers = %v, want xai", trigger.providers)
	}
}

func TestCollectQuotaUnsupportedWithoutTrigger(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/quota/collect", handler.CollectQuota)

	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/quota/collect", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("collect status = %d body=%s", response.Code, response.Body.String())
	}
}
