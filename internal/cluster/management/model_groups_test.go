package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
)

func TestModelGroupDetailRoutesCreateAndClearChannels(t *testing.T) {
	handler, closeRepo := newUsageObservabilityTestHandler(t)
	defer closeRepo()

	ctx := t.Context()
	channel, errChannel := handler.repo.CreateChannelGroup(ctx, "codex-subset", false)
	if errChannel != nil {
		t.Fatalf("CreateChannelGroup() error = %v", errChannel)
	}
	modelGroup, errModelGroup := handler.repo.CreateModelGroup(ctx, "codex-models", false)
	if errModelGroup != nil {
		t.Fatalf("CreateModelGroup() error = %v", errModelGroup)
	}

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.GET("/model-group-details", handler.ListModelGroupDetails)
	engine.POST("/model-group-details", handler.CreateModelGroupDetail)
	engine.PATCH("/model-group-details/:id", handler.UpdateModelGroupDetail)

	invalidBody, errInvalidBody := json.Marshal(map[string]any{
		"model_group_id": modelGroup.ID,
		"model_id":       "gpt-invalid",
		"channels":       []uint{channel.ID, 0},
	})
	if errInvalidBody != nil {
		t.Fatalf("marshal invalid body: %v", errInvalidBody)
	}
	invalidResponse := httptest.NewRecorder()
	invalidRequest := httptest.NewRequest(http.MethodPost, "/model-group-details", bytes.NewReader(invalidBody))
	invalidRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d body=%s, want %d", invalidResponse.Code, invalidResponse.Body.String(), http.StatusBadRequest)
	}

	createBody, errCreateBody := json.Marshal(map[string]any{
		"model_group_id": modelGroup.ID,
		"model_id":       "gpt-5.4",
		"channels":       []uint{channel.ID},
	})
	if errCreateBody != nil {
		t.Fatalf("marshal create body: %v", errCreateBody)
	}
	createResponse := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/model-group-details", bytes.NewReader(createBody))
	createRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var createPayload struct {
		Detail struct {
			ID       uint   `json:"id"`
			Channels []uint `json:"channels"`
		} `json:"model_group_detail"`
	}
	if errDecode := json.Unmarshal(createResponse.Body.Bytes(), &createPayload); errDecode != nil {
		t.Fatalf("decode create response: %v", errDecode)
	}
	if createPayload.Detail.ID == 0 || !reflect.DeepEqual(createPayload.Detail.Channels, []uint{channel.ID}) {
		t.Fatalf("created detail = %#v", createPayload.Detail)
	}

	filterResponse := httptest.NewRecorder()
	filterRequest := httptest.NewRequest(http.MethodGet, "/model-group-details?model_id=gpt-5.4(high)", nil)
	engine.ServeHTTP(filterResponse, filterRequest)
	if filterResponse.Code != http.StatusOK {
		t.Fatalf("filter status = %d body=%s", filterResponse.Code, filterResponse.Body.String())
	}
	var filterPayload struct {
		Details []struct {
			ID      uint   `json:"id"`
			ModelID string `json:"model_id"`
		} `json:"model_group_details"`
	}
	if errDecode := json.Unmarshal(filterResponse.Body.Bytes(), &filterPayload); errDecode != nil {
		t.Fatalf("decode filter response: %v", errDecode)
	}
	if len(filterPayload.Details) != 1 || filterPayload.Details[0].ID != createPayload.Detail.ID || filterPayload.Details[0].ModelID != "gpt-5.4" {
		t.Fatalf("filtered details = %#v, want canonical created detail", filterPayload.Details)
	}

	invalidPatchResponse := httptest.NewRecorder()
	invalidPatchRequest := httptest.NewRequest(http.MethodPatch, "/model-group-details/"+modelRecordID(createPayload.Detail.ID), bytes.NewBufferString(`{"channels":[0]}`))
	invalidPatchRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(invalidPatchResponse, invalidPatchRequest)
	if invalidPatchResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid patch status = %d body=%s, want %d", invalidPatchResponse.Code, invalidPatchResponse.Body.String(), http.StatusBadRequest)
	}

	patchResponse := httptest.NewRecorder()
	patchRequest := httptest.NewRequest(http.MethodPatch, "/model-group-details/"+modelRecordID(createPayload.Detail.ID), bytes.NewBufferString(`{"channels":[]}`))
	patchRequest.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(patchResponse, patchRequest)
	if patchResponse.Code != http.StatusOK {
		t.Fatalf("patch status = %d body=%s", patchResponse.Code, patchResponse.Body.String())
	}
	var patchPayload struct {
		Detail struct {
			Channels []uint `json:"channels"`
		} `json:"model_group_detail"`
	}
	if errDecode := json.Unmarshal(patchResponse.Body.Bytes(), &patchPayload); errDecode != nil {
		t.Fatalf("decode patch response: %v", errDecode)
	}
	if patchPayload.Detail.Channels == nil || len(patchPayload.Detail.Channels) != 0 {
		t.Fatalf("patched channels = %#v, want empty array", patchPayload.Detail.Channels)
	}
}

func modelRecordID(id uint) string {
	return strconv.FormatUint(uint64(id), 10)
}

func TestModelGroupDetailRecordToMapIncludesChannels(t *testing.T) {
	t.Parallel()

	item, errItem := modelGroupDetailRecordToMap(&cluster.ModelGroupDetailRecord{
		ID:           10,
		ModelGroupID: 2,
		ModelID:      "gpt-5.4(high)",
		Channels:     cluster.JSONB(`[4,2,4]`),
	})
	if errItem != nil {
		t.Fatalf("modelGroupDetailRecordToMap() error = %v", errItem)
	}
	if item["model_id"] != "gpt-5.4" {
		t.Fatalf("model_id = %v, want canonical gpt-5.4", item["model_id"])
	}
	channels, ok := item["channels"].([]uint)
	if !ok {
		t.Fatalf("channels = %T, want []uint", item["channels"])
	}
	if want := []uint{2, 4}; !reflect.DeepEqual(channels, want) {
		t.Fatalf("channels = %v, want %v", channels, want)
	}
}
