package management

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPIHome/internal/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"github.com/router-for-me/CLIProxyAPIHome/internal/watcher/synthesizer"
)

// ListAuthFiles returns an auth files.
func (h *Handler) ListAuthFiles(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuths)
		return
	}
	observation, errObservation := h.repo.ReadInFlightObservation(ctx, h.inFlightStaleAfter())
	var observationRead *cluster.InFlightObservationReadModel
	if errObservation == nil {
		observationRead = &observation
	}
	states, errStates := h.repo.ReadConcurrencyState(ctx)
	limiterAvailable := errStates == nil
	observedCredentials := map[string]cluster.InFlightObservedCredentialItem{}
	if observationRead != nil {
		observedCredentials = observedCredentialsByID(*observationRead)
	}
	statesByCredential := make(map[string]cluster.CredentialConcurrencyState, len(states))
	for index := range states {
		state := states[index]
		statesByCredential[state.CredentialID] = state
	}
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if !isOAuthAuth(auth) {
			continue
		}
		entry := authFileEntry(auth)
		observed, observedOK := observedCredentials[auth.ID]
		if observationRead != nil && observedOK {
			applyInFlightObservationToAuthFile(entry, *observationRead, observed)
		} else {
			applyNoInFlightObservationToAuthFile(entry)
		}
		if limiterAvailable {
			applyConcurrencyStateToAuthFile(entry, statesByCredential[auth.ID], observationRead, observed, observedOK)
		}
		files = append(files, entry)
	}
	sort.Slice(files, func(i, j int) bool {
		return fmt.Sprint(files[i]["name"]) < fmt.Sprint(files[j]["name"])
	})
	var observedAt any
	if observationRead != nil {
		observedAt = inFlightObservedAt(*observationRead)
	}
	c.JSON(http.StatusOK, gin.H{"in_flight_observed_at": observedAt, "files": files})
}

// DownloadAuthFile downloads an auth file.
func (h *Handler) DownloadAuthFile(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	auth, errAuth := h.findOAuthAuth(ctx, authIdentifierFromRequest(c))
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		respondError(c, http.StatusNotFound, "not_found", nil)
		return
	}
	data, errMarshal := json.MarshalIndent(auth.Metadata, "", "  ")
	if errMarshal != nil {
		respondError(c, http.StatusInternalServerError, "marshal_failed", errMarshal)
		return
	}
	data = append(data, '\n')
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", authFileName(auth)))
	_, _ = c.Writer.Write(data)
}

// UploadAuthFile handles upload auth file.
func (h *Handler) UploadAuthFile(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	if strings.HasPrefix(strings.ToLower(c.ContentType()), "multipart/form-data") {
		headers, errHeaders := multipartHeaders(c)
		if errHeaders != nil {
			respondError(c, http.StatusBadRequest, "invalid multipart", errHeaders)
			return
		}
		if len(headers) == 0 {
			respondError(c, http.StatusBadRequest, "no files uploaded", nil)
			return
		}
		uploaded := make([]string, 0, len(headers))
		for _, header := range headers {
			name, errStore := h.storeUploadedOAuth(c, header)
			if errStore != nil {
				respondError(c, http.StatusBadRequest, "upload_failed", errStore)
				return
			}
			uploaded = append(uploaded, name)
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
		return
	}
	body, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errRead)
		return
	}
	name, errStore := h.storeOAuthPayload(c, body, "")
	if errStore != nil {
		respondError(c, http.StatusBadRequest, "upload_failed", errStore)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "name": name})
}

// DeleteAuthFile deletes an auth file.
func (h *Handler) DeleteAuthFile(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	ctx, cancel := h.requestContext(c)
	defer cancel()
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuths)
		return
	}
	if all := strings.TrimSpace(c.Query("all")); all == "1" || strings.EqualFold(all, "true") || all == "*" {
		deleted := 0
		for _, auth := range auths {
			if !isOAuthAuth(auth) {
				continue
			}
			if errDelete := h.repo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
				respondError(c, http.StatusInternalServerError, "write_failed", errDelete)
				return
			}
			deleted++
		}
		if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
			respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": deleted})
		return
	}

	auth := findOAuthAuthInList(auths, authIdentifierFromRequest(c))
	if auth == nil {
		respondError(c, http.StatusNotFound, "not_found", nil)
		return
	}
	if errDelete := h.deleteOAuthAuth(ctx, auth); errDelete != nil {
		respondError(c, http.StatusInternalServerError, "write_failed", errDelete)
		return
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// PatchAuthFileStatus applies a partial update to an auth file status.
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	var req struct {
		Name     string `json:"name"`
		Disabled *bool  `json:"disabled"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	auth, errAuth := h.findOAuthAuth(ctx, authIdentifier{ID: name, Name: name})
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	disabled := *req.Disabled
	applyAuthDisabledStatus(auth, disabled)
	if _, errUpsert := h.repo.UpsertAuth(ctx, auth, "update"); errUpsert != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errUpsert)})
		return
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", errRefresh)})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": disabled})
}

// PatchAuthFileFields applies a partial update to an auth file fields.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	// Validate request inputs before mutating persisted state.
	var body map[string]json.RawMessage
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	if errDecode := decoder.Decode(&body); errDecode != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errDecode)
		return
	}
	values, errValues := decodeOAuthFieldPatchValues(body)
	if errValues != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errValues)
		return
	}
	concurrencyPatch, errConcurrencyPatch := decodeConcurrencyPolicyPatchRequest(body)
	if errConcurrencyPatch != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errConcurrencyPatch)
		return
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()
	auth, errAuth := h.findOAuthAuth(ctx, authIdentifierFromBodyAndRequest(c, values))
	if errAuth != nil {
		respondError(c, http.StatusInternalServerError, "auth_load_failed", errAuth)
		return
	}
	if auth == nil {
		respondError(c, http.StatusNotFound, "not_found", nil)
		return
	}
	removeOAuthFieldPatchIdentifierFields(body)
	removeOAuthFieldPatchConcurrencyFields(body)
	changed, errPatch := applyOAuthFieldPatch(auth, body)
	if errPatch != nil {
		respondError(c, http.StatusBadRequest, "invalid body", errPatch)
		return
	}
	policyChanged := concurrencyPatch.MaxInFlight.Set || concurrencyPatch.MaxInFlightByModel.Set
	if !changed && !policyChanged {
		respondError(c, http.StatusBadRequest, "no fields to update", nil)
		return
	}
	if changed {
		auth.UpdatedAt = time.Now().UTC()
	}
	errWrite := h.repo.PatchAuthAndConcurrency(ctx, cluster.AuthConcurrencyPatchRequest{
		CredentialID:          auth.ID,
		Auth:                  auth,
		AuthChanged:           changed,
		PolicyPatch:           cluster.ConcurrencyPolicyPatch{MaxInFlight: concurrencyPatch.MaxInFlight, MaxInFlightByModel: concurrencyPatch.MaxInFlightByModel},
		ExpectedPolicyVersion: concurrencyPatch.Version,
	})
	if errWrite != nil {
		respondConcurrencyPolicyError(c, errWrite)
		return
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return
	}
	respondOK(c)
}

// PostOAuthCallback handles a post o auth callback.
func (h *Handler) PostOAuthCallback(c *gin.Context) {
	h.handleOAuthCallback(c)
}

// storeUploadedOAuth stores an uploaded o auth.
func (h *Handler) storeUploadedOAuth(c *gin.Context, header *multipart.FileHeader) (string, error) {
	if header == nil {
		return "", fmt.Errorf("file is required")
	}
	if !strings.HasSuffix(strings.ToLower(filepath.Base(header.Filename)), ".json") {
		return "", fmt.Errorf("file must be .json")
	}
	src, errOpen := header.Open()
	if errOpen != nil {
		return "", errOpen
	}
	data, errRead := io.ReadAll(src)
	if errClose := src.Close(); errClose != nil && errRead == nil {
		errRead = errClose
	}
	if errRead != nil {
		return "", errRead
	}
	return h.storeOAuthPayload(c, data, header.Filename)
}

// storeOAuthPayload stores an o auth payload.
func (h *Handler) storeOAuthPayload(c *gin.Context, raw []byte, originalFilename string) (string, error) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	return h.storeOAuthPayloadWithContext(ctx, raw, originalFilename)
}

// storeOAuthPayloadWithContext stores an o auth payload with context.
func (h *Handler) storeOAuthPayloadWithContext(ctx context.Context, raw []byte, originalFilename string) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return "", fmt.Errorf("empty credential json")
	}
	updatedRaw, fileUUID, _, errUUID := cluster.EnsureOAuthPayloadUUID(raw)
	if errUUID != nil {
		return "", errUUID
	}

	auths := h.synthesizeOAuthPayload(updatedRaw, fileUUID, originalFilename)
	if len(auths) == 0 {
		return "", fmt.Errorf("unsupported credential json")
	}
	if errReplace := h.replaceOAuthPayloadAuths(ctx, fileUUID, auths); errReplace != nil {
		return "", errReplace
	}
	if errRefresh := h.refreshAuths(ctx); errRefresh != nil {
		return "", errRefresh
	}
	return fileUUID + ".json", nil
}

// synthesizeOAuthPayload handles a synthesize o auth payload.
func (h *Handler) synthesizeOAuthPayload(raw []byte, fileUUID string, originalFilename string) []*coreauth.Auth {
	// Resolve credential context before calling upstream OAuth services.
	cfg := h.runtime.Config()
	authPath := fileUUID + ".json"
	sctx := &synthesizer.SynthesisContext{
		Config:      cfg,
		AuthDir:     "",
		Now:         time.Now().UTC(),
		IDGenerator: synthesizer.NewStableIDGenerator(),
		ClusterMode: true,
	}
	sctx.UUIDForAuth = func(auth *coreauth.Auth) string {
		_ = auth
		return fileUUID
	}
	auths := synthesizer.SynthesizeAuthFile(sctx, authPath, raw)
	cluster.ApplyOriginalAuthFileName(auths, originalFilename)
	return auths
}

// replaceOAuthPayloadAuths handles a replace o auth payload auths.
func (h *Handler) replaceOAuthPayloadAuths(ctx context.Context, fileUUID string, auths []*coreauth.Auth) error {
	// Resolve credential context before calling upstream OAuth services.
	nextIDs := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		nextIDs[auth.ID] = struct{}{}
		if _, errUpsert := h.repo.UpsertAuth(ctx, auth, "upsert"); errUpsert != nil {
			return errUpsert
		}
	}

	existingAuths, errExisting := h.repo.ListAuths(ctx)
	if errExisting != nil {
		return errExisting
	}
	for _, existing := range existingAuths {
		if !isOAuthPayloadAuth(existing, fileUUID) {
			continue
		}
		if _, ok := nextIDs[existing.ID]; ok {
			continue
		}
		if errDelete := h.repo.SoftDeleteAuth(ctx, existing.ID); errDelete != nil {
			return errDelete
		}
	}
	return nil
}

// isOAuthPayloadAuth reports whether o auth payload auth.
func isOAuthPayloadAuth(auth *coreauth.Auth, fileUUID string) bool {
	fileUUID = strings.TrimSpace(fileUUID)
	if auth == nil || fileUUID == "" {
		return false
	}
	if strings.TrimSpace(auth.ID) == fileUUID {
		return true
	}
	return false
}

// findOAuthAuth handles a find o auth auth.
func (h *Handler) findOAuthAuth(ctx context.Context, identifier authIdentifier) (*coreauth.Auth, error) {
	auths, errAuths := h.repo.ListAuths(ctx)
	if errAuths != nil {
		return nil, errAuths
	}
	return findOAuthAuthInList(auths, identifier), nil
}

// deleteOAuthAuth deletes an o auth auth.
func (h *Handler) deleteOAuthAuth(ctx context.Context, auth *coreauth.Auth) error {
	if auth == nil {
		return nil
	}
	if errDelete := h.repo.SoftDeleteAuth(ctx, auth.ID); errDelete != nil {
		return errDelete
	}
	return nil
}

type authIdentifier struct {
	ID    string
	Name  string
	Index *int
}

// authIdentifierFromRequest derives auth identifier from request.
func authIdentifierFromRequest(c *gin.Context) authIdentifier {
	identifier := authIdentifier{
		ID:   firstNonEmptyQuery(c, "id", "uuid", "auth_index"),
		Name: firstNonEmptyQuery(c, "name", "file", "filename"),
	}
	if idxRaw := strings.TrimSpace(c.Query("index")); idxRaw != "" {
		if idx, errAtoi := strconv.Atoi(idxRaw); errAtoi == nil {
			identifier.Index = &idx
		}
	}
	return identifier
}

// authIdentifierFromBodyAndRequest derives auth identifier from body and request.
func authIdentifierFromBodyAndRequest(c *gin.Context, body map[string]any) authIdentifier {
	identifier := authIdentifierFromRequest(c)
	for _, key := range []string{"id", "uuid", "auth_index", "index"} {
		if identifier.ID == "" {
			identifier.ID = stringFromAny(body[key])
		}
	}
	if identifier.Name == "" {
		for _, key := range []string{"name", "file", "filename"} {
			identifier.Name = stringFromAny(body[key])
			if identifier.Name != "" {
				break
			}
		}
	}
	return identifier
}

// findOAuthAuthInList handles a find o auth auth in list.
func findOAuthAuthInList(auths []*coreauth.Auth, identifier authIdentifier) *coreauth.Auth {
	// Resolve credential context before calling upstream OAuth services.
	filtered := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if isOAuthAuth(auth) {
			filtered = append(filtered, auth)
		}
	}
	if identifier.Index != nil && *identifier.Index >= 0 && *identifier.Index < len(filtered) {
		return filtered[*identifier.Index]
	}
	for _, auth := range filtered {
		if identifier.ID != "" && (auth.ID == identifier.ID || auth.Index == identifier.ID) {
			return auth
		}
	}
	if identifier.Name != "" {
		for _, auth := range filtered {
			if authFileDisplayName(auth) == identifier.Name {
				return auth
			}
		}
		for _, auth := range filtered {
			if authFileName(auth) == identifier.Name {
				return auth
			}
		}
	}
	for _, auth := range filtered {
		if identifier.Name != "" && authFileName(auth) == identifier.Name {
			return auth
		}
	}
	return nil
}

// isOAuthAuth reports whether o auth auth.
func isOAuthAuth(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	if auth.Attributes != nil && strings.HasPrefix(strings.TrimSpace(auth.Attributes["source"]), "config:") {
		return false
	}
	typeValue := stringFromAny(auth.Metadata["type"])
	return typeValue != ""
}

// applyAuthDisabledStatus updates disabled status fields and persisted metadata.
func applyAuthDisabledStatus(auth *coreauth.Auth, disabled bool) {
	if auth == nil {
		return
	}
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		auth.StatusMessage = "disabled via management API"
	} else {
		auth.Status = coreauth.StatusActive
		auth.StatusMessage = ""
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["disabled"] = disabled
	auth.UpdatedAt = time.Now()
}

// authFileEntry handles an auth file entry.
func authFileEntry(auth *coreauth.Auth) gin.H {
	// Validate request inputs before mutating persisted state.
	entry := gin.H{
		"id":             auth.ID,
		"auth_index":     auth.ID,
		"name":           authFileDisplayName(auth),
		"file_name":      authFileName(auth),
		"type":           auth.Provider,
		"provider":       auth.Provider,
		"label":          auth.Label,
		"status":         auth.Status,
		"status_message": auth.StatusMessage,
		"disabled":       auth.Disabled,
		"unavailable":    auth.Unavailable,
		"runtime_only":   auth.Attributes != nil && strings.EqualFold(auth.Attributes["runtime_only"], "true"),
		"source":         "db",
		"prefix":         prefixFromAuth(auth),
		"proxy_url":      proxyURLFromAuth(auth),
		"websockets":     websocketsFromAuth(auth),
	}
	if email := stringFromAny(auth.Metadata["email"]); email != "" {
		entry["email"] = email
	}
	if priority := priorityFromAuth(auth); priority != nil {
		entry["priority"] = *priority
	}
	if note := noteFromAuth(auth); note != "" {
		entry["note"] = note
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["updated_at"] = auth.UpdatedAt
		entry["modtime"] = auth.UpdatedAt
	}
	return entry
}

// authFileName handles an auth file name.
func authFileName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if name := stringFromAny(auth.Metadata["filename"]); name != "" {
			return name
		}
	}
	return strings.TrimSpace(auth.ID) + ".json"
}

// authFileDisplayName returns a stable management-list name for the auth entry.
func authFileDisplayName(auth *coreauth.Auth) string {
	return authFileName(auth)
}

// applyOAuthFieldPatch applies an o auth field patch.
func applyOAuthFieldPatch(auth *coreauth.Auth, fields map[string]json.RawMessage) (bool, error) {
	// Resolve credential context before calling upstream OAuth services.
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	changed := false
	touchedRoots := make(map[string]struct{}, len(fields))
	for key, rawValue := range fields {
		fieldPath := strings.TrimSpace(key)
		if fieldPath == "" {
			return false, fmt.Errorf("field name is required")
		}
		value, errDecode := decodeOAuthFieldPatchValue(rawValue)
		if errDecode != nil {
			return false, fmt.Errorf("invalid field %s", fieldPath)
		}
		metadataPath := oauthMetadataFieldPath(fieldPath)
		if metadataPath == "headers" {
			applyOAuthHeadersPatch(auth, value)
		} else if errSet := setOAuthMetadataValue(auth.Metadata, metadataPath, value); errSet != nil {
			return false, errSet
		}
		if root := rootOAuthField(metadataPath); root != "" {
			touchedRoots[root] = struct{}{}
		}
		changed = true
	}
	if changed {
		syncOAuthMetadataFields(auth, touchedRoots)
	}
	return changed, nil
}

func decodeOAuthFieldPatchValues(fields map[string]json.RawMessage) (map[string]any, error) {
	values := make(map[string]any, len(fields))
	for key, rawValue := range fields {
		value, errDecode := decodeOAuthFieldPatchValue(rawValue)
		if errDecode != nil {
			return nil, fmt.Errorf("invalid field %s", key)
		}
		values[key] = value
	}
	return values, nil
}

func decodeOAuthFieldPatchValue(raw json.RawMessage) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if errDecode := decoder.Decode(&value); errDecode != nil {
		return nil, errDecode
	}
	return value, nil
}

func removeOAuthFieldPatchIdentifierFields(fields map[string]json.RawMessage) {
	for _, key := range []string{"id", "uuid", "auth_index", "index", "name", "file", "filename"} {
		delete(fields, key)
	}
}

func removeOAuthFieldPatchConcurrencyFields(fields map[string]json.RawMessage) {
	for _, key := range []string{"version", "max_in_flight", "max_in_flight_by_model"} {
		delete(fields, key)
	}
}

func oauthMetadataFieldPath(path string) string {
	if strings.TrimSpace(path) == "proxy-url" {
		return "proxy_url"
	}
	return path
}

func rootOAuthField(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if idx := strings.Index(path, "."); idx >= 0 {
		return strings.TrimSpace(path[:idx])
	}
	return path
}

func setOAuthMetadataValue(metadata map[string]any, path string, value any) error {
	if metadata == nil {
		return fmt.Errorf("metadata is nil")
	}
	parts := strings.Split(path, ".")
	current := metadata
	for i, rawPart := range parts {
		part := strings.TrimSpace(rawPart)
		if part == "" {
			return fmt.Errorf("invalid field path: %s", path)
		}
		if i == len(parts)-1 {
			current[part] = value
			return nil
		}
		next, ok := current[part].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		current = next
	}
	return nil
}

func applyOAuthHeadersPatch(auth *coreauth.Auth, value any) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	headers, ok := oauthHeadersStringMap(value)
	if !ok {
		auth.Metadata["headers"] = value
		return
	}
	currentHeaders := coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata)
	nextHeaders := make(map[string]string, len(currentHeaders))
	for name, value := range currentHeaders {
		nextHeaders[name] = value
	}
	for key, value := range headers {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		headerValue := strings.TrimSpace(value)
		if headerValue == "" {
			delete(nextHeaders, name)
			continue
		}
		nextHeaders[name] = headerValue
	}
	if len(nextHeaders) == 0 {
		delete(auth.Metadata, "headers")
		return
	}
	metadataHeaders := make(map[string]any, len(nextHeaders))
	for name, value := range nextHeaders {
		metadataHeaders[name] = value
	}
	auth.Metadata["headers"] = metadataHeaders
}

func oauthHeadersStringMap(value any) (map[string]string, bool) {
	switch typed := value.(type) {
	case map[string]string:
		return typed, true
	case map[string]any:
		headers := make(map[string]string, len(typed))
		for name, rawValue := range typed {
			headerValue, ok := rawValue.(string)
			if !ok {
				return nil, false
			}
			headers[name] = headerValue
		}
		return headers, true
	default:
		return nil, false
	}
}

func syncOAuthMetadataFields(auth *coreauth.Auth, touchedRoots map[string]struct{}) {
	if auth == nil || len(touchedRoots) == 0 {
		return
	}
	if _, ok := touchedRoots["prefix"]; ok {
		if prefix, okString := auth.Metadata["prefix"].(string); okString {
			auth.Prefix = strings.TrimSpace(prefix)
		}
	}
	if _, ok := touchedRoots["proxy_url"]; ok {
		if proxyURL, okString := auth.Metadata["proxy_url"].(string); okString {
			auth.ProxyURL = strings.TrimSpace(proxyURL)
		}
	}
	if _, ok := touchedRoots["headers"]; ok {
		syncOAuthHeaderAttributes(auth)
	}
	if _, ok := touchedRoots["priority"]; ok {
		syncOAuthPriorityAttribute(auth)
	}
	if _, ok := touchedRoots["note"]; ok {
		syncOAuthNoteAttribute(auth)
	}
	if _, ok := touchedRoots["websockets"]; ok {
		syncOAuthWebsocketsAttribute(auth)
	}
	if _, ok := touchedRoots["disabled"]; ok {
		syncOAuthDisabledState(auth)
	}
}

func syncOAuthHeaderAttributes(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	for key := range auth.Attributes {
		if strings.HasPrefix(key, "header:") {
			delete(auth.Attributes, key)
		}
	}
	for name, value := range coreauth.ExtractCustomHeadersFromMetadata(auth.Metadata) {
		auth.Attributes["header:"+name] = value
	}
}

func syncOAuthPriorityAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	priority, ok := oauthIntValue(auth.Metadata["priority"])
	if !ok || priority == 0 {
		delete(auth.Attributes, "priority")
		return
	}
	auth.Attributes["priority"] = strconv.Itoa(priority)
}

func oauthIntValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		if i, errInt := typed.Int64(); errInt == nil {
			return int(i), true
		}
	case string:
		if i, errAtoi := strconv.Atoi(strings.TrimSpace(typed)); errAtoi == nil {
			return i, true
		}
	}
	return 0, false
}

func syncOAuthNoteAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	note, ok := auth.Metadata["note"].(string)
	if !ok {
		delete(auth.Attributes, "note")
		return
	}
	note = strings.TrimSpace(note)
	if note == "" {
		delete(auth.Attributes, "note")
		return
	}
	auth.Attributes["note"] = note
}

func syncOAuthWebsocketsAttribute(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if auth.Attributes == nil {
		auth.Attributes = make(map[string]string)
	}
	websockets, ok := oauthBoolValue(auth.Metadata["websockets"])
	if !ok {
		delete(auth.Attributes, "websockets")
		return
	}
	auth.Attributes["websockets"] = strconv.FormatBool(websockets)
}

func oauthBoolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(typed))
		if errParse == nil {
			return parsed, true
		}
	}
	return false, false
}

func syncOAuthDisabledState(auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	disabled, ok := oauthBoolValue(auth.Metadata["disabled"])
	if !ok {
		return
	}
	auth.Disabled = disabled
	if disabled {
		auth.Status = coreauth.StatusDisabled
		if strings.TrimSpace(auth.StatusMessage) == "" {
			auth.StatusMessage = "disabled via management API"
		}
		return
	}
	auth.Status = coreauth.StatusActive
	auth.StatusMessage = ""
}

// multipartHeaders handles a multipart headers.
func multipartHeaders(c *gin.Context) ([]*multipart.FileHeader, error) {
	form, errForm := c.MultipartForm()
	if errForm != nil {
		return nil, errForm
	}
	if form == nil {
		return nil, nil
	}
	keys := make([]string, 0, len(form.File))
	for key := range form.File {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	headers := make([]*multipart.FileHeader, 0)
	for _, key := range keys {
		headers = append(headers, form.File[key]...)
	}
	return headers, nil
}

// stringFromAny derives string from any.
func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

// priorityFromAuth derives priority from auth.
func priorityFromAuth(auth *coreauth.Auth) *int {
	if auth == nil {
		return nil
	}
	if auth.Attributes != nil {
		if priorityRaw := strings.TrimSpace(auth.Attributes["priority"]); priorityRaw != "" {
			if priority, errAtoi := strconv.Atoi(priorityRaw); errAtoi == nil {
				return &priority
			}
		}
	}
	if auth.Metadata != nil {
		if priorityRaw, ok := auth.Metadata["priority"].(float64); ok {
			priority := int(priorityRaw)
			return &priority
		}
	}
	return nil
}

func prefixFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if prefix := strings.TrimSpace(auth.Prefix); prefix != "" {
		return prefix
	}
	if auth.Metadata != nil {
		return stringFromAny(auth.Metadata["prefix"])
	}
	return ""
}

func proxyURLFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if proxyURL := strings.TrimSpace(auth.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	if auth.Metadata != nil {
		if proxyURL := stringFromAny(auth.Metadata["proxy_url"]); proxyURL != "" {
			return proxyURL
		}
		return stringFromAny(auth.Metadata["proxy-url"])
	}
	return ""
}

func websocketsFromAuth(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if auth.Attributes != nil {
		if websockets, ok := oauthBoolValue(auth.Attributes["websockets"]); ok {
			return websockets
		}
	}
	if auth.Metadata != nil {
		if websockets, ok := oauthBoolValue(auth.Metadata["websockets"]); ok {
			return websockets
		}
	}
	return false
}

// noteFromAuth derives note from auth.
func noteFromAuth(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if note := strings.TrimSpace(auth.Attributes["note"]); note != "" {
			return note
		}
	}
	if auth.Metadata != nil {
		return stringFromAny(auth.Metadata["note"])
	}
	return ""
}
