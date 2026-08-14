package management

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/cluster"
	"gorm.io/gorm"
)

const apiKeyMaxRequestBodySize int64 = 1 << 20

type apiKeyRequestError struct {
	code string
	err  error
}

func bytesTrimSpace(data []byte) []byte {
	return bytes.TrimSpace(data)
}

func decodeStrictAPIKeyJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if errDecode := decoder.Decode(target); errDecode != nil {
		return errDecode
	}
	var trailing any
	if errTrailing := decoder.Decode(&trailing); !errors.Is(errTrailing, io.EOF) {
		if errTrailing == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return errTrailing
	}
	return nil
}

func (e *apiKeyRequestError) Error() string {
	if e == nil || e.err == nil {
		return "invalid API key request"
	}
	return e.err.Error()
}

func (e *apiKeyRequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newAPIKeyRequestError(code string, format string, args ...any) error {
	return &apiKeyRequestError{code: code, err: fmt.Errorf(format, args...)}
}

type apiKeyEntryBody struct {
	APIKey          *string         `json:"api_key"`
	APIKeyDash      *string         `json:"api-key"`
	Key             *string         `json:"key"`
	Value           *string         `json:"value"`
	DisplayName     json.RawMessage `json:"display_name"`
	UserID          json.RawMessage `json:"user_id"`
	UserIDDash      json.RawMessage `json:"user-id"`
	Channels        *[]uint         `json:"channels"`
	ModelGroups     *[]uint         `json:"model_groups"`
	ModelGroupsDash *[]uint         `json:"model-groups"`
}

type apiKeyPatchBody struct {
	ID              *uint           `json:"id"`
	APIKeyID        *uint           `json:"api_key_id"`
	APIKeyIDDash    *uint           `json:"api-key-id"`
	Old             *string         `json:"old"`
	New             *string         `json:"new"`
	Index           *int            `json:"index"`
	Value           json.RawMessage `json:"value"`
	APIKey          *string         `json:"api_key"`
	APIKeyDash      *string         `json:"api-key"`
	Key             *string         `json:"key"`
	DisplayName     json.RawMessage `json:"display_name"`
	UserID          json.RawMessage `json:"user_id"`
	UserIDDash      json.RawMessage `json:"user-id"`
	Channels        *[]uint         `json:"channels"`
	ModelGroups     *[]uint         `json:"model_groups"`
	ModelGroupsDash *[]uint         `json:"model-groups"`
}

func (b apiKeyPatchBody) selectorAPIKey() (string, error) {
	values := make([]string, 0, 3)
	for _, value := range []*string{b.APIKey, b.APIKeyDash, b.Key} {
		if value == nil {
			continue
		}
		key := strings.TrimSpace(*value)
		if key == "" {
			return "", newAPIKeyRequestError("invalid_body", "API key selector aliases must not be blank")
		}
		values = append(values, key)
	}
	if len(values) == 0 {
		return "", nil
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "", newAPIKeyRequestError("invalid_body", "conflicting API key selector aliases")
		}
	}
	return values[0], nil
}

func (b apiKeyPatchBody) selectorID() (*uint, error) {
	var selected *uint
	for _, value := range []*uint{b.ID, b.APIKeyID, b.APIKeyIDDash} {
		if value == nil {
			continue
		}
		if selected != nil && *selected != *value {
			return nil, newAPIKeyRequestError("invalid_body", "conflicting API key ID aliases")
		}
		selected = value
	}
	return selected, nil
}

func apiKeyEntriesResponse(entries []cluster.APIKeyEntry) gin.H {
	keys := make([]string, 0, len(entries))
	items := make([]gin.H, 0, len(entries))
	for _, entry := range entries {
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		channels := append([]uint(nil), entry.Channels...)
		if channels == nil {
			channels = []uint{}
		}
		modelGroups := append([]uint(nil), entry.ModelGroups...)
		if modelGroups == nil {
			modelGroups = []uint{}
		}
		keys = append(keys, key)
		items = append(items, gin.H{
			"id":           entry.ID,
			"api_key_id":   entry.ID,
			"api-key":      key,
			"api_key":      key,
			"display_name": optionalAPIKeyDisplayNameValue(entry.DisplayName),
			"user-id":      optionalUserIDValue(entry.UserID),
			"user_id":      optionalUserIDValue(entry.UserID),
			"channels":     channels,
			"model_groups": modelGroups,
		})
	}
	return gin.H{
		"api-keys":        keys,
		"items":           items,
		"api_key_entries": items,
	}
}

func decodeAPIKeyEntryUpdates(data []byte) ([]cluster.APIKeyEntryUpdate, error) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || string(data) == "null" {
		return nil, newAPIKeyRequestError("invalid_body", "API key replacement body must be an array or object wrapper")
	}
	if data[0] == '[' {
		entries, errEntries := decodeAPIKeyEntryArray(data)
		if errEntries != nil {
			return nil, errEntries
		}
		return entries, nil
	}
	if data[0] != '{' {
		return nil, newAPIKeyRequestError("invalid_body", "API key replacement body must be an array or object wrapper")
	}

	var wrapper map[string]json.RawMessage
	if errUnmarshal := decodeStrictAPIKeyJSON(data, &wrapper); errUnmarshal != nil {
		return nil, newAPIKeyRequestError("invalid_body", "invalid API key replacement body: %v", errUnmarshal)
	}
	var selected json.RawMessage
	selectedKey := ""
	for _, key := range []string{"items", "api-keys", "api_keys", "api_key_entries"} {
		raw, present := wrapper[key]
		if !present {
			continue
		}
		if selectedKey != "" {
			return nil, newAPIKeyRequestError("invalid_body", "API key replacement body must contain exactly one wrapper field")
		}
		selectedKey = key
		selected = raw
	}
	if selectedKey == "" {
		return nil, newAPIKeyRequestError("invalid_body", "missing API key replacement items")
	}
	if string(bytesTrimSpace(selected)) == "null" {
		return nil, newAPIKeyRequestError("invalid_body", "%s must be an array", selectedKey)
	}
	return decodeAPIKeyEntryArray(selected)
}

func decodeAPIKeyEntryArray(data []byte) ([]cluster.APIKeyEntryUpdate, error) {
	var rawItems []json.RawMessage
	if errUnmarshal := decodeStrictAPIKeyJSON(data, &rawItems); errUnmarshal != nil {
		return nil, newAPIKeyRequestError("invalid_body", "API key replacement items must be an array: %v", errUnmarshal)
	}
	if rawItems == nil {
		return nil, newAPIKeyRequestError("invalid_body", "API key replacement items must be an array")
	}
	entries := make([]cluster.APIKeyEntryUpdate, 0, len(rawItems))
	for index, raw := range rawItems {
		entry, errEntry := decodeAPIKeyEntry(raw)
		if errEntry != nil {
			return nil, fmt.Errorf("API key replacement item %d: %w", index, errEntry)
		}
		if strings.TrimSpace(entry.APIKey) == "" {
			return nil, newAPIKeyRequestError("invalid_body", "API key replacement item %d is missing a non-empty key", index)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func decodeAPIKeyEntry(data []byte) (cluster.APIKeyEntryUpdate, error) {
	data = bytesTrimSpace(data)
	if len(data) == 0 || string(data) == "null" {
		return cluster.APIKeyEntryUpdate{}, newAPIKeyRequestError("invalid_body", "API key entry must be a non-empty string or object")
	}
	var key string
	if data[0] == '"' {
		if errString := json.Unmarshal(data, &key); errString != nil {
			return cluster.APIKeyEntryUpdate{}, newAPIKeyRequestError("invalid_body", "invalid API key string: %v", errString)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return cluster.APIKeyEntryUpdate{}, newAPIKeyRequestError("invalid_body", "API key entry must not be blank")
		}
		return cluster.APIKeyEntryUpdate{APIKey: key}, nil
	}
	if data[0] != '{' {
		return cluster.APIKeyEntryUpdate{}, newAPIKeyRequestError("invalid_body", "API key entry must be a non-empty string or object")
	}

	var body apiKeyEntryBody
	if errUnmarshal := decodeStrictAPIKeyJSON(data, &body); errUnmarshal != nil {
		return cluster.APIKeyEntryUpdate{}, newAPIKeyRequestError("invalid_body", "invalid API key entry: %v", errUnmarshal)
	}
	apiKey, errAPIKey := body.apiKey()
	if errAPIKey != nil {
		return cluster.APIKeyEntryUpdate{}, errAPIKey
	}
	displayName, displayNameSet, errDisplayName := decodeOptionalAPIKeyDisplayName(body.DisplayName)
	if errDisplayName != nil {
		return cluster.APIKeyEntryUpdate{}, errDisplayName
	}
	userID, userIDSet, errUserID := decodeOptionalAPIKeyUserIDAliases(body.UserID, body.UserIDDash)
	if errUserID != nil {
		return cluster.APIKeyEntryUpdate{}, errUserID
	}
	modelGroups, errModelGroups := body.modelGroups()
	if errModelGroups != nil {
		return cluster.APIKeyEntryUpdate{}, errModelGroups
	}
	return cluster.APIKeyEntryUpdate{
		APIKey:         apiKey,
		DisplayName:    displayName,
		DisplayNameSet: displayNameSet,
		UserID:         userID,
		UserIDSet:      userIDSet,
		Channels:       body.Channels,
		ModelGroups:    modelGroups,
	}, nil
}

func decodeOptionalAPIKeyDisplayName(raw json.RawMessage) (*string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if !utf8.Valid(raw) {
		return nil, false, newAPIKeyRequestError("invalid_display_name", "display_name must contain valid UTF-8")
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value string
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return nil, false, newAPIKeyRequestError("invalid_display_name", "display_name must be a string or null")
	}
	normalized, errDisplayName := validateAPIKeyDisplayName(&value)
	if errDisplayName != nil {
		return nil, false, errDisplayName
	}
	return normalized, true, nil
}

func validateAPIKeyDisplayName(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	if !utf8.ValidString(*value) {
		return nil, newAPIKeyRequestError("invalid_display_name", "display_name must contain valid UTF-8")
	}
	for _, character := range *value {
		if unicode.IsControl(character) {
			return nil, newAPIKeyRequestError("invalid_display_name", "display_name must not contain control characters")
		}
	}
	trimmed := strings.TrimSpace(*value)
	if utf8.RuneCountInString(trimmed) > cluster.APIKeyDisplayNameMaxLength {
		return nil, newAPIKeyRequestError("invalid_display_name", "display_name must not exceed %d characters", cluster.APIKeyDisplayNameMaxLength)
	}
	return &trimmed, nil
}

func decodeOptionalAPIKeyUserID(raw json.RawMessage) (*uint, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value uint
	if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal != nil {
		return nil, false, fmt.Errorf("user_id must be a non-negative integer or null")
	}
	return &value, true, nil
}

func decodeOptionalAPIKeyUserIDAliases(values ...json.RawMessage) (*uint, bool, error) {
	var selected *uint
	selectedSet := false
	for _, raw := range values {
		if len(raw) == 0 {
			continue
		}
		value, valueSet, errValue := decodeOptionalAPIKeyUserID(raw)
		if errValue != nil {
			return nil, false, errValue
		}
		if !selectedSet {
			selected = value
			selectedSet = valueSet
			continue
		}
		if !sameAPIKeyRequestUserID(selected, value) {
			return nil, false, newAPIKeyRequestError("invalid_body", "conflicting user ID aliases")
		}
	}
	return selected, selectedSet, nil
}

func sameAPIKeyRequestUserID(left *uint, right *uint) bool {
	if left == nil || *left == 0 {
		return right == nil || *right == 0
	}
	return right != nil && *right == *left
}

func (b apiKeyEntryBody) apiKey() (string, error) {
	values := make([]string, 0, 4)
	for _, value := range []*string{b.APIKey, b.APIKeyDash, b.Key, b.Value} {
		if value == nil {
			continue
		}
		key := strings.TrimSpace(*value)
		if key == "" {
			return "", newAPIKeyRequestError("invalid_body", "API key aliases must not be blank")
		}
		values = append(values, key)
	}
	if len(values) == 0 {
		return "", newAPIKeyRequestError("invalid_body", "API key entry is missing a non-empty key")
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "", newAPIKeyRequestError("invalid_body", "conflicting API key aliases")
		}
	}
	return values[0], nil
}

func (b apiKeyEntryBody) modelGroups() (*[]uint, error) {
	if b.ModelGroups != nil && b.ModelGroupsDash != nil && !sameAPIKeyRequestIDs(*b.ModelGroups, *b.ModelGroupsDash) {
		return nil, newAPIKeyRequestError("invalid_body", "conflicting model group aliases")
	}
	if b.ModelGroups != nil {
		return b.ModelGroups, nil
	}
	return b.ModelGroupsDash, nil
}

func (h *Handler) createAPIKeyEntry(c *gin.Context) {
	limitAPIKeyRequestBody(c)
	data, errData := c.GetRawData()
	if errData != nil {
		respondAPIKeyRequestError(c, errData)
		return
	}
	entry, errEntry := decodeAPIKeyEntry(data)
	if errEntry != nil {
		respondAPIKeyRequestError(c, errEntry)
		return
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()
	record, runtimeChanged, errCreate := h.repo.CreateAPIKeyWithRuntimeChange(ctx, entry)
	if errCreate != nil {
		respondAPIKeyMutationError(c, errCreate)
		return
	}
	if runtimeChanged {
		if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
			respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
			return
		}
	}
	responseEntry, errResponseEntry := clusterAPIKeyEntryFromRecord(record)
	if errResponseEntry != nil {
		respondError(c, http.StatusInternalServerError, "api_key_load_failed", errResponseEntry)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"api_key": apiKeyEntryToMap(responseEntry)})
}

func (h *Handler) patchAPIKeyEntries(c *gin.Context) error {
	limitAPIKeyRequestBody(c)
	var body apiKeyPatchBody
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(errBindJSON, &maxBytesError) {
			return errBindJSON
		}
		return newAPIKeyRequestError("invalid_body", "invalid API key patch body: %v", errBindJSON)
	}
	selectorID, errSelectorID := body.selectorID()
	if errSelectorID != nil {
		return errSelectorID
	}
	if selectorID != nil && (body.Index != nil || body.Old != nil) {
		return fmt.Errorf("invalid api key selector")
	}
	if body.Index != nil && (body.Old != nil || body.New != nil) {
		return fmt.Errorf("invalid api key selector")
	}
	if body.Old != nil && body.New == nil {
		return fmt.Errorf("old requires new")
	}

	ctx, cancel := h.requestContext(c)
	defer cancel()

	selector := cluster.APIKeySelector{}
	update := cluster.APIKeyAdminUpdate{}
	appendIfMissing := false

	if body.Index != nil && len(body.Value) > 0 {
		if *body.Index < 0 {
			return fmt.Errorf("invalid index")
		}
		selector.Index = body.Index
		next, errEntry := decodeAPIKeyEntry(body.Value)
		if errEntry != nil {
			return errEntry
		}
		if strings.TrimSpace(next.APIKey) == "" {
			return fmt.Errorf("invalid value")
		}
		applyAPIKeyEntryUpdate(&update, next)
	} else if body.Old != nil && body.New != nil {
		oldKey := strings.TrimSpace(*body.Old)
		newKey := strings.TrimSpace(*body.New)
		if oldKey == "" || newKey == "" {
			return fmt.Errorf("missing fields")
		}
		selector.APIKey = oldKey
		update.APIKey = &newKey
		appendIfMissing = true
	} else {
		selectorKey, errSelectorKey := body.selectorAPIKey()
		if errSelectorKey != nil {
			return errSelectorKey
		}
		if id := selectorID; id != nil {
			if *id == 0 {
				return fmt.Errorf("invalid id")
			}
			selector.ID = *id
			selector.APIKey = selectorKey
		} else {
			selector.APIKey = selectorKey
		}
		if len(body.Value) > 0 {
			next, errEntry := decodeAPIKeyEntry(body.Value)
			if errEntry != nil {
				return errEntry
			}
			applyAPIKeyEntryUpdate(&update, next)
		}
		if body.New != nil {
			newKey := strings.TrimSpace(*body.New)
			if newKey == "" {
				return fmt.Errorf("invalid new value")
			}
			update.APIKey = &newKey
		}
		userID, userIDSet, errUserID := decodeOptionalAPIKeyUserIDAliases(body.UserID, body.UserIDDash)
		if errUserID != nil {
			return errUserID
		}
		if userIDSet {
			if userID == nil {
				zero := uint(0)
				userID = &zero
			}
			update.UserID = userID
		}
		if body.Channels != nil {
			update.Channels = body.Channels
		}
		modelGroups, errModelGroups := body.modelGroups()
		if errModelGroups != nil {
			return errModelGroups
		}
		if modelGroups != nil {
			update.ModelGroups = modelGroups
		}
	}
	displayName, displayNameSet, errDisplayName := decodeOptionalAPIKeyDisplayName(body.DisplayName)
	if errDisplayName != nil {
		return errDisplayName
	}
	if displayNameSet {
		update.DisplayName = displayName
		update.DisplayNameSet = true
	}

	if update.APIKey == nil && !update.DisplayNameSet && update.UserID == nil && update.Channels == nil && update.ModelGroups == nil {
		return fmt.Errorf("missing fields")
	}
	if selector.ID == 0 && selector.Index == nil && strings.TrimSpace(selector.APIKey) == "" {
		return fmt.Errorf("missing api key selector")
	}

	record, runtimeChanged, errUpdate := h.repo.UpdateAPIKeyWithRuntimeChange(ctx, selector, update)
	if errUpdate != nil && appendIfMissing && errors.Is(errUpdate, gorm.ErrRecordNotFound) {
		record, runtimeChanged, errUpdate = h.repo.CreateAPIKeyWithRuntimeChange(ctx, cluster.APIKeyEntryUpdate{
			APIKey:         *update.APIKey,
			DisplayName:    update.DisplayName,
			DisplayNameSet: update.DisplayNameSet,
		})
	}
	if errUpdate != nil {
		respondAPIKeyMutationError(c, errUpdate)
		return nil
	}
	if runtimeChanged {
		if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
			respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
			return nil
		}
	}
	responseEntry, errResponseEntry := clusterAPIKeyEntryFromRecord(record)
	if errResponseEntry != nil {
		respondError(c, http.StatusInternalServerError, "api_key_load_failed", errResponseEntry)
		return nil
	}
	c.JSON(http.StatusOK, gin.H{"api_key": apiKeyEntryToMap(responseEntry)})
	return nil
}

func (h *Handler) deleteAPIKeyEntry(c *gin.Context) error {
	selector := cluster.APIKeySelector{}
	if idRaw := firstNonEmptyQuery(c, "id", "api_key_id", "api-key-id"); idRaw != "" {
		id, errID := strconv.ParseUint(idRaw, 10, 64)
		if errID != nil || id == 0 {
			return fmt.Errorf("invalid id")
		}
		selector.ID = uint(id)
		selector.APIKey = firstNonEmptyQuery(c, "value", "api_key", "api-key", "key")
	}
	if idxRaw := c.Query("index"); idxRaw != "" {
		index, errIndex := strconv.Atoi(idxRaw)
		if errIndex != nil || index < 0 {
			return fmt.Errorf("invalid index")
		}
		selector.Index = &index
	}
	if selector.ID == 0 && selector.Index == nil {
		selector.APIKey = firstNonEmptyQuery(c, "value", "api_key", "api-key", "key")
	}
	if selector.ID == 0 && selector.Index == nil && strings.TrimSpace(selector.APIKey) == "" {
		return fmt.Errorf("missing id, index, or value")
	}
	ctx, cancel := h.requestContext(c)
	defer cancel()

	if errDelete := h.repo.DeleteAPIKey(ctx, selector); errDelete != nil {
		respondAPIKeyMutationError(c, errDelete)
		return nil
	}
	if errRefresh := h.refreshConfig(ctx); errRefresh != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", errRefresh)
		return nil
	}
	respondOK(c)
	return nil
}

func applyAPIKeyEntryUpdate(update *cluster.APIKeyAdminUpdate, entry cluster.APIKeyEntryUpdate) {
	if update == nil {
		return
	}
	if key := strings.TrimSpace(entry.APIKey); key != "" {
		update.APIKey = &key
	}
	if entry.DisplayNameSet {
		update.DisplayName = entry.DisplayName
		update.DisplayNameSet = true
	}
	if entry.UserIDSet || entry.UserID != nil {
		if entry.UserID == nil {
			zero := uint(0)
			update.UserID = &zero
		} else {
			update.UserID = entry.UserID
		}
	}
	if entry.Channels != nil {
		update.Channels = entry.Channels
	}
	if entry.ModelGroups != nil {
		update.ModelGroups = entry.ModelGroups
	}
}

func respondAPIKeyMutationError(c *gin.Context, err error) {
	switch {
	case cluster.IsAPIKeyConflictError(err):
		respondError(c, http.StatusConflict, "api_key_exists", err)
	case errors.Is(err, cluster.ErrUserNotFound):
		respondError(c, http.StatusNotFound, "user_not_found", err)
	case errors.Is(err, gorm.ErrRecordNotFound):
		respondError(c, http.StatusNotFound, "api_key_not_found", err)
	case errors.Is(err, cluster.ErrAPIKeySelectorMismatch):
		respondError(c, http.StatusBadRequest, "invalid_api_key_selector", err)
	case errors.Is(err, cluster.ErrInvalidAPIKeyDisplayName):
		respondError(c, http.StatusBadRequest, "invalid_display_name", err)
	default:
		respondError(c, http.StatusInternalServerError, "write_failed", err)
	}
}

func limitAPIKeyRequestBody(c *gin.Context) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, apiKeyMaxRequestBodySize)
}

func respondAPIKeyRequestError(c *gin.Context, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		respondError(c, http.StatusRequestEntityTooLarge, "request_body_too_large", fmt.Errorf("API key request body exceeds %d bytes", apiKeyMaxRequestBodySize))
		return
	}
	requestError := &apiKeyRequestError{}
	if errors.As(err, &requestError) {
		respondError(c, http.StatusBadRequest, requestError.code, requestError.err)
		return
	}
	respondError(c, http.StatusBadRequest, "invalid_body", err)
}

func clusterAPIKeyEntryFromRecord(record *cluster.APIKeyRecord) (cluster.APIKeyEntry, error) {
	return cluster.APIKeyEntryFromRecord(record)
}

func apiKeyEntryToMap(entry cluster.APIKeyEntry) gin.H {
	channels := append([]uint(nil), entry.Channels...)
	if channels == nil {
		channels = []uint{}
	}
	modelGroups := append([]uint(nil), entry.ModelGroups...)
	if modelGroups == nil {
		modelGroups = []uint{}
	}
	return gin.H{
		"id":           entry.ID,
		"api_key_id":   entry.ID,
		"api-key":      strings.TrimSpace(entry.APIKey),
		"api_key":      strings.TrimSpace(entry.APIKey),
		"display_name": optionalAPIKeyDisplayNameValue(entry.DisplayName),
		"user-id":      optionalUserIDValue(entry.UserID),
		"user_id":      optionalUserIDValue(entry.UserID),
		"channels":     channels,
		"model_groups": modelGroups,
	}
}

func optionalAPIKeyDisplayNameValue(displayName *string) any {
	if displayName == nil {
		return nil
	}
	value := strings.TrimSpace(*displayName)
	if value == "" {
		return nil
	}
	return value
}

func (b apiKeyPatchBody) modelGroups() (*[]uint, error) {
	if b.ModelGroups != nil && b.ModelGroupsDash != nil && !sameAPIKeyRequestIDs(*b.ModelGroups, *b.ModelGroupsDash) {
		return nil, newAPIKeyRequestError("invalid_body", "conflicting model group aliases")
	}
	if b.ModelGroups != nil {
		return b.ModelGroups, nil
	}
	return b.ModelGroupsDash, nil
}

func sameAPIKeyRequestIDs(left []uint, right []uint) bool {
	normalize := func(values []uint) []uint {
		seen := make(map[uint]struct{}, len(values))
		out := make([]uint, 0, len(values))
		for _, value := range values {
			if value == 0 {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			out = append(out, value)
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	return reflect.DeepEqual(normalize(left), normalize(right))
}

func normalizeAPIKeyEntryUserID(userID *uint) *uint {
	if userID == nil || *userID == 0 {
		return nil
	}
	value := *userID
	return &value
}

func optionalUserIDValue(userID *uint) any {
	userID = normalizeAPIKeyEntryUserID(userID)
	if userID == nil {
		return nil
	}
	return *userID
}
