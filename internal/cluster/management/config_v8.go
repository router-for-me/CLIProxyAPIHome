package management

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

// ConfigV8 serves the persisted configuration using v8 names. Mutations read and
// update the authoritative database view in one transaction before reloading runtime state.
func (h *Handler) ConfigV8(c *gin.Context) {
	ctx, cancel := h.requestContext(c)
	defer cancel()
	path := strings.Trim(c.Param("path"), "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	yamlRequest := strings.HasSuffix(c.FullPath(), "/config.yaml")
	if c.Request.Method == http.MethodGet {
		stored, err := h.configRoot(ctx)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "config_load_failed", err)
			return
		}
		if err = h.applyCredentialConfig(ctx, stored); err != nil {
			respondError(c, http.StatusInternalServerError, "auth_load_failed", err)
			return
		}
		doc, data, err := configV8Document(stored)
		if err != nil {
			respondError(c, http.StatusInternalServerError, "invalid_config", err)
			return
		}
		root := doc.Content[0]
		config.ProjectV8ConfigAliases(root, strings.Join(parts, "."))
		if !yamlRequest {
			if servers := configV8Node(root, v8ICEServersPath); servers != nil && servers.Kind == yaml.SequenceNode {
				for _, server := range servers.Content {
					deleteConfigV8Path(server, []string{"username"})
					deleteConfigV8Path(server, []string{"credential"})
				}
			}
			injectConfigV8APIKeyAuthIndexes(root)
		}
		value := configV8Node(root, parts)
		if value == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
			return
		}
		c.Header("Cache-Control", "no-store")
		if yamlRequest {
			c.Data(http.StatusOK, "application/yaml; charset=utf-8", data)
			return
		}
		var result any
		if err = value.Decode(&result); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "decode_failed"})
			return
		}
		c.JSON(http.StatusOK, result)
		return
	}
	// Read the request before acquiring database locks so a slow upload cannot hold them.
	var update *yaml.Node
	if c.Request.Method != http.MethodDelete {
		body, errRead := io.ReadAll(c.Request.Body)
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
			return
		}
		if !yamlRequest && !json.Valid(body) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_json"})
			return
		}
		var input yaml.Node
		if errDecode := yaml.Unmarshal(body, &input); errDecode != nil || len(input.Content) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_body"})
			return
		}
		update = input.Content[0]
		stripConfigV8AuthIndexes(parts, update)
		if len(parts) == 0 && update.Kind != yaml.MappingNode {
			c.JSON(http.StatusBadRequest, gin.H{"error": "config_must_be_object"})
			return
		}
	}
	credentialsChanged := false
	err := h.repo.MutateConfigSnapshot(ctx, h.heartbeatTimeout, func(stored map[string]any) (map[string]any, error) {
		doc, _, errDocument := configV8Document(stored)
		if errDocument != nil {
			return nil, &configV8ResponseError{http.StatusInternalServerError, gin.H{"error": "invalid_config", "message": errDocument.Error()}}
		}
		values, changed, errUpdate := updateConfigV8Document(doc, parts, update, c.Request.Method, yamlRequest)
		credentialsChanged = changed
		return values, errUpdate
	})
	if err != nil {
		var responseError *configV8ResponseError
		if errors.As(err, &responseError) {
			c.JSON(responseError.status, responseError.body)
		} else {
			respondConfigWriteError(c, err)
		}
		return
	}
	if err = h.refreshConfig(ctx); err != nil {
		respondError(c, http.StatusInternalServerError, "reload_failed", err)
		return
	}
	if credentialsChanged {
		if err = h.refreshAuths(ctx); err != nil {
			respondError(c, http.StatusInternalServerError, "reload_failed", err)
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "config-version": 8})
}

// configV8Document produces the same normalized view for reads and transactional edits.
func configV8Document(stored map[string]any) (*yaml.Node, []byte, error) {
	config.ApplyHomeRuntimeScalars(stored)
	data, err := yaml.Marshal(stored)
	if err != nil {
		return nil, nil, err
	}
	data, _, err = config.NormalizeConfigLayout(data, true)
	if err != nil {
		return nil, nil, err
	}
	var doc yaml.Node
	if err = yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	return &doc, data, nil
}

// configV8ResponseError carries validation responses across the transaction boundary.
type configV8ResponseError struct {
	status int
	body   gin.H
}

func (e *configV8ResponseError) Error() string { code, _ := e.body["error"].(string); return code }

func updateConfigV8Document(doc *yaml.Node, parts []string, update *yaml.Node, method string, yamlRequest bool) (map[string]any, bool, error) {
	root := doc.Content[0]
	before := cloneConfigV8Node(root)
	config.ProjectV8ConfigAliases(root, strings.Join(parts, "."))
	if method == http.MethodDelete {
		if len(parts) == 0 {
			return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "cannot_delete_config"}}
		}
		if !deleteConfigV8Path(root, parts) {
			return nil, false, &configV8ResponseError{http.StatusNotFound, gin.H{"error": "not_found"}}
		}
	} else {
		if len(parts) == 0 {
			update = cloneConfigV8Node(update)
			if err := config.NormalizeV8ConfigAliases(update); err != nil {
				return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()}}
			}
		}
		// Paths identify YAML keys, never array indexes. Lists are replaced whole.
		dst := root
		for _, part := range parts {
			if part == "" || dst.Kind != yaml.MappingNode {
				return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "invalid_path"}}
			}
			next := configV8Node(dst, []string{part})
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				dst.Content = append(dst.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
			}
			dst = next
		}
		if method == http.MethodPatch {
			mergeConfigV8Patch(dst, update)
		} else {
			*dst = *cloneConfigV8Node(update)
		}
	}
	if err := config.NormalizeV8ConfigAliases(root); err != nil {
		return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()}}
	}
	if !yamlRequest && method != http.MethodDelete {
		preserveV8TURNSecrets(root, before)
	}
	// Revisions are owned by Home and must not become ordinary editable settings.
	for _, field := range []string{"credentials/concurrency/lifecycle-config-revision", "credentials/concurrency/observation-barrier-revision", "plugins/auth-revision"} {
		old := configV8Node(before, strings.Split(field, "/"))
		next := configV8Node(root, strings.Split(field, "/"))
		var a, b any
		if old != nil {
			_ = old.Decode(&a)
		}
		if next != nil {
			_ = next.Decode(&b)
		}
		if !reflect.DeepEqual(a, b) {
			return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "read_only_field", "field": field}}
		}
	}
	data, err := yaml.Marshal(doc)
	if err == nil {
		err = config.ValidateV8Config(data)
	}
	if err != nil {
		return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()}}
	}
	var values map[string]any
	if err = root.Decode(&values); err != nil {
		return nil, false, &configV8ResponseError{http.StatusBadRequest, gin.H{"error": "invalid_config", "message": err.Error()}}
	}
	values, err = config.NormalizeConfigRoot(values)
	if err != nil {
		return nil, false, &configV8ResponseError{http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()}}
	}
	var previous map[string]any
	if err = before.Decode(&previous); err == nil {
		previous, err = config.NormalizeConfigRoot(previous)
	}
	if err != nil {
		return nil, false, &configV8ResponseError{http.StatusInternalServerError, gin.H{"error": "config_load_failed", "message": err.Error()}}
	}
	// Unchanged families can be omitted; individual unchanged credentials in a
	// changed family are preserved by repository reconciliation.
	credentialsChanged := false
	for _, key := range []string{"gemini-api-key", "interactions-api-key", "vertex-api-key", "codex-api-key", "xai-api-key", "meta-api-key", "claude-api-key", "openai-compatibility"} {
		if reflect.DeepEqual(previous[key], values[key]) {
			delete(values, key)
			continue
		}
		credentialsChanged = true
		if _, retained := values[key]; !retained {
			values[key] = []any{}
		}
	}
	config.ApplyHomeRuntimeScalars(values)
	if _, err = configFromRoot(values); err != nil {
		return nil, false, &configV8ResponseError{http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()}}
	}
	return values, credentialsChanged, nil
}

var v8ICEServersPath = []string{"oauth", "providers", "codex", "live-media-relay", "ice-servers"}

// JSON reads redact TURN secrets. Preserve omitted credentials when a client
// writes that JSON back, matching by endpoint rather than array position so a
// changed URL cannot inherit credentials for a different server. Explicit empty
// strings/null clear a secret; YAML writes retain full replacement semantics.
func preserveV8TURNSecrets(root, before *yaml.Node) {
	next := configV8Node(root, v8ICEServersPath)
	previous := configV8Node(before, v8ICEServersPath)
	if next == nil || previous == nil || next.Kind != yaml.SequenceNode || previous.Kind != yaml.SequenceNode {
		return
	}
	matched := make([]bool, len(previous.Content))
	for _, server := range next.Content {
		urls := configV8Node(server, []string{"urls"})
		if urls == nil {
			continue
		}
		var newURLs []string
		if urls.Decode(&newURLs) != nil {
			continue
		}
		for i, old := range previous.Content {
			if matched[i] {
				continue
			}
			oldURLs := configV8Node(old, []string{"urls"})
			var previousURLs []string
			if oldURLs == nil || oldURLs.Decode(&previousURLs) != nil || !reflect.DeepEqual(newURLs, previousURLs) {
				continue
			}
			matched[i] = true
			for _, name := range []string{"username", "credential"} {
				if configV8Node(server, []string{name}) == nil {
					if secret := configV8Node(old, []string{name}); secret != nil {
						server.Content = append(server.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name}, cloneConfigV8Node(secret))
					}
				}
			}
			break
		}
	}
}

func configV8Node(root *yaml.Node, parts []string) *yaml.Node {
	for _, part := range parts {
		if root == nil || root.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == part {
				next = root.Content[i+1]
				break
			}
		}
		root = next
	}
	return root
}

// deleteConfigV8Path prunes only empty ancestors of the removed field. Other
// explicit empty maps can carry inheritance or plugin semantics and stay intact.
func deleteConfigV8Path(root *yaml.Node, parts []string) bool {
	if root == nil || root.Kind != yaml.MappingNode || len(parts) == 0 {
		return false
	}
	for i := 0; i < len(root.Content); i += 2 {
		if root.Content[i].Value != parts[0] {
			continue
		}
		if len(parts) > 1 {
			child := root.Content[i+1]
			if !deleteConfigV8Path(child, parts[1:]) {
				return false
			}
			if len(child.Content) > 0 {
				return true
			}
		}
		root.Content = append(root.Content[:i], root.Content[i+2:]...)
		return true
	}
	return false
}

func cloneConfigV8Node(node *yaml.Node) *yaml.Node {
	copy := *node
	copy.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		copy.Content[i] = cloneConfigV8Node(child)
	}
	return &copy
}

// Unlike JSON merge-patch, null is retained: optional key overrides use it to
// inherit the group value. DELETE is the explicit field-removal operation.
func mergeConfigV8Patch(dst, src *yaml.Node) {
	if dst.Kind != yaml.MappingNode || src.Kind != yaml.MappingNode {
		*dst = *src
		return
	}
	for i := 0; i < len(src.Content); i += 2 {
		key, value := src.Content[i], src.Content[i+1]
		if old := configV8Node(dst, []string{key.Value}); old != nil {
			mergeConfigV8Patch(old, value)
		} else {
			dst.Content = append(dst.Content, key, value)
		}
	}
}
