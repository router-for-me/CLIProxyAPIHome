package registry

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

//go:embed models/devin_models.json
var embeddedDevinModelsJSON []byte

type devinModelsFilePayload struct {
	Devin  []*ModelInfo `json:"devin,omitempty"`
	Models []*ModelInfo `json:"models,omitempty"`
}

type devinModelsStore struct {
	mu       sync.RWMutex
	models   []*ModelInfo
	rawJSON  []byte
	revision uint64
}

var devinCatalogStore = &devinModelsStore{}

func init() {
	if _, err := loadDevinModelsFromBytes(embeddedDevinModelsJSON, "embed"); err != nil {
		log.Warnf("registry: failed to parse embedded devin_models.json (will rely on legacy definitions and remote refresh): %v", err)
	}
}

// GetDevinModels returns a copy of the active Devin model catalog.
func GetDevinModels() []*ModelInfo {
	return cloneModelInfos(devinModelDefinitions())
}

// devinModelDefinitions returns read-only definitions from the active snapshot,
// with the legacy models.json section available as a fallback.
func devinModelDefinitions() []*ModelInfo {
	devinCatalogStore.mu.RLock()
	models := devinCatalogStore.models
	devinCatalogStore.mu.RUnlock()

	if len(models) > 0 {
		return models
	}

	if m := getModels(); m != nil && len(m.Devin) > 0 {
		models = cloneModelInfos(m.Devin)
		for _, model := range models {
			if model != nil {
				model.ID = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model.ID)), "devin/")
			}
		}
		return models
	}

	return nil
}

func loadDevinModelsFromBytes(data []byte, source string) (bool, error) {
	models, err := ValidateDevinModelsJSON(data)
	if err != nil {
		return false, fmt.Errorf("%s: %w", source, err)
	}

	clonedData := append([]byte(nil), data...)
	devinCatalogStore.mu.Lock()
	if bytes.Equal(devinCatalogStore.rawJSON, clonedData) {
		devinCatalogStore.mu.Unlock()
		return false, nil
	}
	devinCatalogStore.models = models
	devinCatalogStore.rawJSON = clonedData
	devinCatalogStore.revision++
	devinCatalogStore.mu.Unlock()

	return true, nil
}

// ValidateDevinModelsJSON parses and validates a Devin model catalog payload.
// Accepts {"devin": [...]}, {"models": [...]}, or a direct JSON array of ModelInfo.
func ValidateDevinModelsJSON(data []byte) ([]*ModelInfo, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("empty Devin models payload")
	}

	var payload devinModelsFilePayload
	if err := json.Unmarshal(data, &payload); err == nil {
		candidates := payload.Devin
		if len(candidates) == 0 {
			candidates = payload.Models
		}
		if len(candidates) > 0 {
			return sanitizeAndValidateDevinModels(candidates)
		}
	}

	var rawList []*ModelInfo
	if err := json.Unmarshal(data, &rawList); err == nil && len(rawList) > 0 {
		return sanitizeAndValidateDevinModels(rawList)
	}

	return nil, fmt.Errorf("invalid Devin models JSON: expected non-empty 'devin'/'models' array or model list")
}

func sanitizeAndValidateDevinModels(models []*ModelInfo) ([]*ModelInfo, error) {
	seen := make(map[string]struct{}, len(models))
	out := make([]*ModelInfo, 0, len(models))

	for i, m := range models {
		if m == nil {
			return nil, fmt.Errorf("model at index %d is null", i)
		}
		id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(m.ID)), "devin/")
		if id == "" {
			return nil, fmt.Errorf("model at index %d has empty id", i)
		}
		cloned := cloneModelInfo(m)
		cloned.ID = id
		if _, exists := seen[id]; exists {
			return nil, fmt.Errorf("duplicate model id: %q", id)
		}
		seen[id] = struct{}{}

		if cloned.Type == "" {
			cloned.Type = "devin"
		}
		if cloned.Object == "" {
			cloned.Object = "model"
		}
		if len(cloned.SupportedInputModalities) == 0 {
			cloned.SupportedInputModalities = []string{"text"}
		}
		if len(cloned.SupportedOutputModalities) == 0 {
			cloned.SupportedOutputModalities = []string{"text"}
		}
		if cloned.InputTokenLimit == 0 && cloned.ContextLength > 0 {
			cloned.InputTokenLimit = cloned.ContextLength
		}
		if cloned.OutputTokenLimit == 0 && cloned.MaxCompletionTokens > 0 {
			cloned.OutputTokenLimit = cloned.MaxCompletionTokens
		}
		if len(cloned.SupportedGenerationMethods) == 0 {
			cloned.SupportedGenerationMethods = []string{"generateContent", "countTokens"}
		}
		out = append(out, cloned)
	}

	return out, nil
}
