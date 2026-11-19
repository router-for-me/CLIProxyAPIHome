package management

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

type channelGroupWriteRequest struct {
	ChannelName *string `json:"channel_name"`
	Name        *string `json:"name"`
	Disabled    *bool   `json:"disabled"`
	Enabled     *bool   `json:"enabled"`
}

type channelGroupDetailWriteRequest struct {
	ChannelGroupID *uint   `json:"channel_group_id"`
	AuthID         *string `json:"auth_id"`
}

// ListChannelGroups returns channel groups.
func (h *Handler) ListChannelGroups(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()

	records, errRecords := h.repo.ListChannelGroups(ctx)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "channel_group_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for _, record := range records {
		items = append(items, channelGroupRecordToMap(&record))
	}
	c.JSON(http.StatusOK, gin.H{"channel_groups": items})
}

// GetChannelGroup returns a channel group.
func (h *Handler) GetChannelGroup(c *gin.Context) {
	id, ok := channelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.GetChannelGroup(ctx, id)
	if errRecord != nil {
		respondChannelRecordError(c, "channel_group_load_failed", errRecord)
		return
	}
	c.JSON(http.StatusOK, gin.H{"channel_group": channelGroupRecordToMap(record)})
}

// CreateChannelGroup creates a channel group.
func (h *Handler) CreateChannelGroup(c *gin.Context) {
	var body channelGroupWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	channelName := body.channelName()
	if channelName == "" {
		respondError(c, http.StatusBadRequest, "invalid body", errRequired("channel_name"))
		return
	}
	disabled, errDisabled := body.disabledValue(false)
	if errDisabled != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errDisabled)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errCreate := h.repo.CreateChannelGroup(ctx, channelName, disabled)
	if errCreate != nil {
		respondError(c, http.StatusInternalServerError, "channel_group_create_failed", errCreate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"channel_group": channelGroupRecordToMap(record)})
}

// UpdateChannelGroup updates a channel group.
func (h *Handler) UpdateChannelGroup(c *gin.Context) {
	id, ok := channelIDFromParam(c)
	if !ok {
		return
	}
	var body channelGroupWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	update := cluster.ChannelGroupUpdate{}
	if body.ChannelName != nil || body.Name != nil {
		channelName := body.channelName()
		update.ChannelName = &channelName
	}
	if body.Disabled != nil || body.Enabled != nil {
		disabled, errDisabled := body.disabledValue(false)
		if errDisabled != nil {
			respondError(c, http.StatusBadRequest, "invalid body", errDisabled)
			return
		}
		update.Disabled = &disabled
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errUpdate := h.repo.UpdateChannelGroup(ctx, id, update)
	if errUpdate != nil {
		respondChannelRecordError(c, "channel_group_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"channel_group": channelGroupRecordToMap(record)})
}

// DeleteChannelGroup deletes a channel group.
func (h *Handler) DeleteChannelGroup(c *gin.Context) {
	id, ok := channelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteChannelGroup(ctx, id); errDelete != nil {
		respondChannelRecordError(c, "channel_group_delete_failed", errDelete)
		return
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// ListChannelGroupDetails returns channel group details.
func (h *Handler) ListChannelGroupDetails(c *gin.Context) {
	filter, ok := channelGroupDetailFilterFromRequest(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	records, errRecords := h.repo.ListChannelGroupDetails(ctx, filter)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "channel_group_detail_load_failed", errRecords)
		return
	}
	h.respondChannelGroupDetails(c, ctx, records, false)
}

// GetChannelGroupDetail returns a channel group detail.
func (h *Handler) GetChannelGroupDetail(c *gin.Context) {
	id, ok := channelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.GetChannelGroupDetail(ctx, id)
	if errRecord != nil {
		respondChannelRecordError(c, "channel_group_detail_load_failed", errRecord)
		return
	}
	h.respondChannelGroupDetails(c, ctx, []cluster.ChannelGroupDetailRecord{*record}, true)
}

func (h *Handler) respondChannelGroupDetails(c *gin.Context, ctx context.Context, records []cluster.ChannelGroupDetailRecord, single bool) {
	response := gin.H{}
	var credentials map[string]cluster.ChannelGroupCredential
	if c.Query("include") == "credential" {
		var errLookup error
		credentials, errLookup = h.repo.ChannelGroupBoundCredentials(ctx, records)
		if errLookup != nil {
			response["credential_metadata_unavailable"] = true
		}
	}
	items := make([]gin.H, 0, len(records))
	for _, record := range records {
		item := channelGroupDetailRecordToMap(&record)
		if credentials != nil {
			if credential, ok := credentials[record.AuthID]; ok {
				item["credential"] = credential
			} else {
				item["credential"] = nil
			}
		}
		items = append(items, item)
	}
	if single {
		response["channel_group_detail"] = items[0]
	} else {
		response["channel_group_details"] = items
	}
	c.JSON(http.StatusOK, response)
}

// ListChannelGroupCredentialCandidates returns the selectable identity fields for the binding editor.
func (h *Handler) ListChannelGroupCredentialCandidates(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	items, errList := h.repo.ListChannelGroupCredentialCandidates(ctx)
	if errList != nil {
		respondError(c, http.StatusInternalServerError, "channel_group_credential_candidates_load_failed", errList)
		return
	}
	c.JSON(http.StatusOK, gin.H{"credential_candidates": items})
}

// CreateChannelGroupDetail creates a channel group detail.
func (h *Handler) CreateChannelGroupDetail(c *gin.Context) {
	var body channelGroupDetailWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	if body.ChannelGroupID == nil || *body.ChannelGroupID == 0 {
		respondError(c, http.StatusBadRequest, "invalid body", errRequired("channel_group_id"))
		return
	}
	authID := ""
	if body.AuthID != nil {
		authID = strings.TrimSpace(*body.AuthID)
	}
	if authID == "" {
		respondError(c, http.StatusBadRequest, "invalid body", errRequired("auth_id"))
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errCreate := h.repo.CreateChannelGroupDetail(ctx, *body.ChannelGroupID, authID)
	if errCreate != nil {
		respondChannelRecordError(c, "channel_group_detail_create_failed", errCreate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"channel_group_detail": channelGroupDetailRecordToMap(record)})
}

// UpdateChannelGroupDetail updates a channel group detail.
func (h *Handler) UpdateChannelGroupDetail(c *gin.Context) {
	id, ok := channelIDFromParam(c)
	if !ok {
		return
	}
	var body channelGroupDetailWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	update := cluster.ChannelGroupDetailUpdate{
		ChannelGroupID: body.ChannelGroupID,
	}
	if body.AuthID != nil {
		authID := strings.TrimSpace(*body.AuthID)
		update.AuthID = &authID
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errUpdate := h.repo.UpdateChannelGroupDetail(ctx, id, update)
	if errUpdate != nil {
		respondChannelRecordError(c, "channel_group_detail_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"channel_group_detail": channelGroupDetailRecordToMap(record)})
}

// DeleteChannelGroupDetail deletes a channel group detail.
func (h *Handler) DeleteChannelGroupDetail(c *gin.Context) {
	id, ok := channelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteChannelGroupDetail(ctx, id); errDelete != nil {
		respondChannelRecordError(c, "channel_group_detail_delete_failed", errDelete)
		return
	}
	respondOK(c)
}

func (r channelGroupWriteRequest) channelName() string {
	if r.ChannelName != nil {
		return strings.TrimSpace(*r.ChannelName)
	}
	if r.Name != nil {
		return strings.TrimSpace(*r.Name)
	}
	return ""
}

func (r channelGroupWriteRequest) disabledValue(defaultValue bool) (bool, error) {
	disabled := defaultValue
	if r.Disabled != nil {
		disabled = *r.Disabled
	}
	if r.Enabled != nil {
		enabledDisabled := !*r.Enabled
		if r.Disabled != nil && disabled != enabledDisabled {
			return false, errConflictingEnabledDisabled()
		}
		disabled = enabledDisabled
	}
	return disabled, nil
}

func channelIDFromParam(c *gin.Context) (uint, bool) {
	id, errID := cluster.ParseChannelRecordID(c.Param("id"))
	if errID != nil {
		respondError(c, http.StatusBadRequest, "invalid id", errID)
		return 0, false
	}
	return id, true
}

func channelGroupDetailFilterFromRequest(c *gin.Context) (cluster.ChannelGroupDetailFilter, bool) {
	filter := cluster.ChannelGroupDetailFilter{
		AuthID: firstNonEmptyQuery(c, "auth_id", "auth-id"),
	}
	groupIDRaw := firstNonEmptyQuery(c, "channel_group_id", "channel-group-id", "group_id", "group-id")
	if groupIDRaw == "" {
		return filter, true
	}
	groupID, errGroupID := cluster.ParseChannelRecordID(groupIDRaw)
	if errGroupID != nil {
		respondError(c, http.StatusBadRequest, "invalid channel_group_id", errGroupID)
		return filter, false
	}
	filter.ChannelGroupID = &groupID
	return filter, true
}

func respondChannelRecordError(c *gin.Context, code string, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, http.StatusNotFound, "not_found", err)
		return
	}
	respondError(c, http.StatusInternalServerError, code, err)
}

func channelGroupRecordToMap(record *cluster.ChannelGroupRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":           record.ID,
		"channel_name": record.ChannelName,
		"disabled":     record.Disabled,
		"enabled":      !record.Disabled,
		"created_at":   record.CreatedAt,
		"updated_at":   record.UpdatedAt,
		"deleted_at":   deletedAtValue(record.DeletedAt),
	}
}

func channelGroupDetailRecordToMap(record *cluster.ChannelGroupDetailRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":               record.ID,
		"channel_group_id": record.ChannelGroupID,
		"auth_id":          record.AuthID,
		"created_at":       record.CreatedAt,
		"updated_at":       record.UpdatedAt,
		"deleted_at":       deletedAtValue(record.DeletedAt),
	}
}

func deletedAtValue(value gorm.DeletedAt) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func errRequired(field string) error {
	return errors.New(field + " is required")
}

func errConflictingEnabledDisabled() error {
	return errors.New("enabled and disabled conflict")
}
