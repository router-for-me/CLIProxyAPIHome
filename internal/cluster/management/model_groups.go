package management

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

type modelGroupWriteRequest struct {
	GroupName *string `json:"group_name"`
	Name      *string `json:"name"`
	Disabled  *bool   `json:"disabled"`
	Enabled   *bool   `json:"enabled"`
}

type modelGroupDetailWriteRequest struct {
	ModelGroupID *uint   `json:"model_group_id"`
	ModelID      *string `json:"model_id"`
	Channels     *[]uint `json:"channels"`
}

// ListModelGroups returns model groups.
func (h *Handler) ListModelGroups(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()

	records, errRecords := h.repo.ListModelGroups(ctx)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "model_group_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for _, record := range records {
		items = append(items, modelGroupRecordToMap(&record))
	}
	c.JSON(http.StatusOK, gin.H{"model_groups": items})
}

// GetModelGroup returns a model group.
func (h *Handler) GetModelGroup(c *gin.Context) {
	id, ok := modelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.GetModelGroup(ctx, id)
	if errRecord != nil {
		respondModelRecordError(c, "model_group_load_failed", errRecord)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_group": modelGroupRecordToMap(record)})
}

// CreateModelGroup creates a model group.
func (h *Handler) CreateModelGroup(c *gin.Context) {
	var body modelGroupWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	groupName := body.groupName()
	if groupName == "" {
		respondError(c, http.StatusBadRequest, "invalid body", errRequired("group_name"))
		return
	}
	disabled, errDisabled := body.disabledValue(false)
	if errDisabled != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errDisabled)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errCreate := h.repo.CreateModelGroup(ctx, groupName, disabled)
	if errCreate != nil {
		respondError(c, http.StatusInternalServerError, "model_group_create_failed", errCreate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_group": modelGroupRecordToMap(record)})
}

// UpdateModelGroup updates a model group.
func (h *Handler) UpdateModelGroup(c *gin.Context) {
	id, ok := modelIDFromParam(c)
	if !ok {
		return
	}
	var body modelGroupWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	update := cluster.ModelGroupUpdate{}
	if body.GroupName != nil || body.Name != nil {
		groupName := body.groupName()
		update.GroupName = &groupName
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
	record, errUpdate := h.repo.UpdateModelGroup(ctx, id, update)
	if errUpdate != nil {
		respondModelRecordError(c, "model_group_update_failed", errUpdate)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_group": modelGroupRecordToMap(record)})
}

// DeleteModelGroup deletes a model group.
func (h *Handler) DeleteModelGroup(c *gin.Context) {
	id, ok := modelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteModelGroup(ctx, id); errDelete != nil {
		respondModelRecordError(c, "model_group_delete_failed", errDelete)
		return
	}
	respondOK(c)
}

// ListModelGroupDetails returns model group details.
func (h *Handler) ListModelGroupDetails(c *gin.Context) {
	filter, ok := modelGroupDetailFilterFromRequest(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	records, errRecords := h.repo.ListModelGroupDetails(ctx, filter)
	if errRecords != nil {
		respondError(c, http.StatusInternalServerError, "model_group_detail_load_failed", errRecords)
		return
	}
	items := make([]gin.H, 0, len(records))
	for _, record := range records {
		item, errItem := modelGroupDetailRecordToMap(&record)
		if errItem != nil {
			respondError(c, http.StatusInternalServerError, "model_group_detail_load_failed", errItem)
			return
		}
		items = append(items, item)
	}
	c.JSON(http.StatusOK, gin.H{"model_group_details": items})
}

// GetModelGroupDetail returns a model group detail.
func (h *Handler) GetModelGroupDetail(c *gin.Context) {
	id, ok := modelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errRecord := h.repo.GetModelGroupDetail(ctx, id)
	if errRecord != nil {
		respondModelRecordError(c, "model_group_detail_load_failed", errRecord)
		return
	}
	item, errItem := modelGroupDetailRecordToMap(record)
	if errItem != nil {
		respondError(c, http.StatusInternalServerError, "model_group_detail_load_failed", errItem)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_group_detail": item})
}

// CreateModelGroupDetail creates a model group detail.
func (h *Handler) CreateModelGroupDetail(c *gin.Context) {
	var body modelGroupDetailWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	if body.ModelGroupID == nil || *body.ModelGroupID == 0 {
		respondError(c, http.StatusBadRequest, "invalid body", errRequired("model_group_id"))
		return
	}
	modelID := ""
	if body.ModelID != nil {
		modelID = strings.TrimSpace(*body.ModelID)
	}
	if modelID == "" {
		respondError(c, http.StatusBadRequest, "invalid body", errRequired("model_id"))
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	channels := []uint(nil)
	if body.Channels != nil {
		channels = *body.Channels
	}
	record, errCreate := h.repo.CreateModelGroupDetail(ctx, *body.ModelGroupID, modelID, channels)
	if errCreate != nil {
		respondModelRecordError(c, "model_group_detail_create_failed", errCreate)
		return
	}
	item, errItem := modelGroupDetailRecordToMap(record)
	if errItem != nil {
		respondError(c, http.StatusInternalServerError, "model_group_detail_create_failed", errItem)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_group_detail": item})
}

// UpdateModelGroupDetail updates a model group detail.
func (h *Handler) UpdateModelGroupDetail(c *gin.Context) {
	id, ok := modelIDFromParam(c)
	if !ok {
		return
	}
	var body modelGroupDetailWriteRequest
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errBindJSON)
		return
	}
	update := cluster.ModelGroupDetailUpdate{
		ModelGroupID: body.ModelGroupID,
		Channels:     body.Channels,
	}
	if body.ModelID != nil {
		modelID := strings.TrimSpace(*body.ModelID)
		update.ModelID = &modelID
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, errUpdate := h.repo.UpdateModelGroupDetail(ctx, id, update)
	if errUpdate != nil {
		respondModelRecordError(c, "model_group_detail_update_failed", errUpdate)
		return
	}
	item, errItem := modelGroupDetailRecordToMap(record)
	if errItem != nil {
		respondError(c, http.StatusInternalServerError, "model_group_detail_update_failed", errItem)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_group_detail": item})
}

// DeleteModelGroupDetail deletes a model group detail.
func (h *Handler) DeleteModelGroupDetail(c *gin.Context) {
	id, ok := modelIDFromParam(c)
	if !ok {
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	if errDelete := h.repo.DeleteModelGroupDetail(ctx, id); errDelete != nil {
		respondModelRecordError(c, "model_group_detail_delete_failed", errDelete)
		return
	}
	respondOK(c)
}

func (r modelGroupWriteRequest) groupName() string {
	if r.GroupName != nil {
		return strings.TrimSpace(*r.GroupName)
	}
	if r.Name != nil {
		return strings.TrimSpace(*r.Name)
	}
	return ""
}

func (r modelGroupWriteRequest) disabledValue(defaultValue bool) (bool, error) {
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

func modelIDFromParam(c *gin.Context) (uint, bool) {
	id, errID := cluster.ParseModelRecordID(c.Param("id"))
	if errID != nil {
		respondError(c, http.StatusBadRequest, "invalid id", errID)
		return 0, false
	}
	return id, true
}

func modelGroupDetailFilterFromRequest(c *gin.Context) (cluster.ModelGroupDetailFilter, bool) {
	filter := cluster.ModelGroupDetailFilter{
		ModelID: firstNonEmptyQuery(c, "model_id", "model-id"),
	}
	groupIDRaw := firstNonEmptyQuery(c, "model_group_id", "model-group-id", "group_id", "group-id")
	if groupIDRaw == "" {
		return filter, true
	}
	groupID, errGroupID := cluster.ParseModelRecordID(groupIDRaw)
	if errGroupID != nil {
		respondError(c, http.StatusBadRequest, "invalid model_group_id", errGroupID)
		return filter, false
	}
	filter.ModelGroupID = &groupID
	return filter, true
}

func respondModelRecordError(c *gin.Context, code string, err error) {
	if errors.Is(err, cluster.ErrInvalidModelChannelGroupID) {
		respondError(c, http.StatusBadRequest, "invalid body", err)
		return
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		respondError(c, http.StatusNotFound, "not_found", err)
		return
	}
	respondError(c, http.StatusInternalServerError, code, err)
}

func modelGroupRecordToMap(record *cluster.ModelGroupRecord) gin.H {
	if record == nil {
		return gin.H{}
	}
	return gin.H{
		"id":         record.ID,
		"group_name": record.GroupName,
		"disabled":   record.Disabled,
		"enabled":    !record.Disabled,
		"created_at": record.CreatedAt,
		"updated_at": record.UpdatedAt,
		"deleted_at": deletedAtValue(record.DeletedAt),
	}
}

func modelGroupDetailRecordToMap(record *cluster.ModelGroupDetailRecord) (gin.H, error) {
	if record == nil {
		return gin.H{}, nil
	}
	channels, errChannels := cluster.ModelGroupDetailChannelIDs(record)
	if errChannels != nil {
		return nil, errChannels
	}
	if channels == nil {
		channels = []uint{}
	}
	return gin.H{
		"id":             record.ID,
		"model_group_id": record.ModelGroupID,
		"model_id":       coreauth.CanonicalModelID(record.ModelID),
		"channels":       channels,
		"created_at":     record.CreatedAt,
		"updated_at":     record.UpdatedAt,
		"deleted_at":     deletedAtValue(record.DeletedAt),
	}, nil
}
