package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestBillingModelPriceExtraPricesCreateAndClear(t *testing.T) {
	t.Parallel()

	handler, closeRepo := newBillingManagementTestHandler(t)
	defer closeRepo()

	createResp := httptest.NewRecorder()
	createCtx, _ := gin.CreateTestContext(createResp)
	createCtx.Request = httptest.NewRequest(http.MethodPost, "/billing/model-prices", bytes.NewBufferString(`{"provider":"claude","model":"claude-x","input_price_per_million":3,"cache_write_1h_price_per_million":6,"reasoning_price_per_million":0}`))
	createCtx.Request.Header.Set("Content-Type", "application/json")
	handler.CreateBillingModelPrice(createCtx)
	if createResp.Code != http.StatusOK {
		t.Fatalf("POST status/body = %d %s", createResp.Code, createResp.Body.String())
	}
	var created struct {
		ModelPrice map[string]any `json:"model_price"`
	}
	if errDecode := json.Unmarshal(createResp.Body.Bytes(), &created); errDecode != nil {
		t.Fatalf("decode create: %v", errDecode)
	}
	if created.ModelPrice["cache_write_1h_price_per_million"] != float64(6) || created.ModelPrice["reasoning_price_per_million"] != float64(0) || created.ModelPrice["image_input_price_per_million"] != nil {
		t.Fatalf("created = %v", created.ModelPrice)
	}

	id, _ := created.ModelPrice["id"].(string)
	patchResp := httptest.NewRecorder()
	patchCtx, _ := gin.CreateTestContext(patchResp)
	patchCtx.Params = gin.Params{{Key: "id", Value: id}}
	patchCtx.Request = httptest.NewRequest(http.MethodPatch, "/billing/model-prices/"+id, bytes.NewBufferString(`{"cache_write_1h_price_per_million":null,"audio_input_price_per_million":4}`))
	patchCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateBillingModelPrice(patchCtx)
	var patched struct {
		ModelPrice map[string]any `json:"model_price"`
	}
	if errDecode := json.Unmarshal(patchResp.Body.Bytes(), &patched); errDecode != nil || patchResp.Code != http.StatusOK {
		t.Fatalf("PATCH status/body = %d %s", patchResp.Code, patchResp.Body.String())
	}
	if patched.ModelPrice["cache_write_1h_price_per_million"] != nil || patched.ModelPrice["audio_input_price_per_million"] != float64(4) || patched.ModelPrice["reasoning_price_per_million"] != float64(0) {
		t.Fatalf("patched = %v", patched.ModelPrice)
	}

	invalidResp := httptest.NewRecorder()
	invalidCtx, _ := gin.CreateTestContext(invalidResp)
	invalidCtx.Params = gin.Params{{Key: "id", Value: id}}
	invalidCtx.Request = httptest.NewRequest(http.MethodPatch, "/billing/model-prices/"+id, bytes.NewBufferString(`{"image_output_price_per_million":-1}`))
	invalidCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateBillingModelPrice(invalidCtx)
	if invalidResp.Code != http.StatusBadRequest {
		t.Fatalf("invalid PATCH status = %d, want 400", invalidResp.Code)
	}
}
