package management

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// QuotaRecollectTrigger starts an asynchronous quota collection round. It is
// implemented by the quota collector and injected at server build time.
type QuotaRecollectTrigger interface {
	TriggerCollection(ctx context.Context, credentialIDs map[string]struct{}, providers map[string]struct{}) (int, error)
}

// quotaRecollectProviders lists the providers with an active quota collector.
var quotaRecollectProviders = map[string]struct{}{
	"claude":      {},
	"antigravity": {},
	"codex":       {},
	"kimi":        {},
	"xai":         {},
}

func (h *Handler) SetQuotaRecollectTrigger(trigger QuotaRecollectTrigger) {
	if h == nil {
		return
	}
	h.quotaRecollect = trigger
}

// CollectQuota handles POST /quota/collect: kick off an asynchronous quota
// collection round for the selected credentials (empty body = all eligible).
func (h *Handler) CollectQuota(c *gin.Context) {
	if h.quotaRecollect == nil {
		respondQuotaHTTPError(c, http.StatusNotFound, "QUOTA_RECOLLECT_UNSUPPORTED", "quota recollection is not available on this runtime", false)
		return
	}
	var body struct {
		CredentialIDs []string `json:"credential_ids"`
		Providers     []string `json:"providers"`
	}
	if c.Request != nil && c.Request.Body != nil {
		decoder := json.NewDecoder(c.Request.Body)
		if errDecode := decoder.Decode(&body); errDecode != nil && !errors.Is(errDecode, io.EOF) {
			respondQuotaHTTPError(c, http.StatusBadRequest, "INVALID_BODY", "quota collect body must be a JSON object", false)
			return
		}
	}
	credentialIDs := make(map[string]struct{}, len(body.CredentialIDs))
	for _, raw := range body.CredentialIDs {
		if value := strings.TrimSpace(raw); value != "" {
			credentialIDs[value] = struct{}{}
		}
	}
	providers := make(map[string]struct{}, len(body.Providers))
	for _, raw := range body.Providers {
		value := strings.ToLower(strings.TrimSpace(raw))
		if value == "" {
			continue
		}
		if value == "grok" {
			value = "xai"
		}
		if _, ok := quotaRecollectProviders[value]; !ok {
			respondQuotaHTTPError(c, http.StatusBadRequest, "INVALID_FILTER", "providers contains an unsupported value: "+value, false)
			return
		}
		providers[value] = struct{}{}
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	accepted, errTrigger := h.quotaRecollect.TriggerCollection(ctx, credentialIDs, providers)
	if errTrigger != nil {
		respondQuotaHTTPError(c, http.StatusInternalServerError, "QUOTA_COLLECT_FAILED", "failed to start quota collection", true)
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"accepted": accepted, "running": accepted > 0})
}
