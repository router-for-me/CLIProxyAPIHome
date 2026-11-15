package diff

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	cpaconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPIHome/internal/config"
	"gopkg.in/yaml.v3"
)

// appendClaudeCloakChanges reads SDK-owned fields without duplicating their schema
// or mutating the stored source tree. Only counts, not rule contents, are logged.
func appendClaudeCloakChanges(changes []string, index int, oldKey, newKey config.ClaudeKey) []string {
	var sdkKeys [2]cpaconfig.ClaudeKey
	for i, entry := range [2]config.ClaudeKey{oldKey, newKey} {
		var source any = entry.Cloak
		if entry.RawConfig != nil {
			source = entry.RawConfig["cloak"]
		}
		var node yaml.Node
		if errEncode := node.Encode(source); errEncode != nil {
			return append(changes, fmt.Sprintf("claude[%d].cloak: invalid configuration (comparison skipped)", index))
		}
		if errDecode := node.Decode(&sdkKeys[i].Cloak); errDecode != nil {
			// Decoder errors can include sensitive rule text; do not log them.
			return append(changes, fmt.Sprintf("claude[%d].cloak: invalid configuration (comparison skipped)", index))
		}
	}

	o, n := sdkKeys[0].Cloak, sdkKeys[1].Cloak
	switch {
	case o == nil && n != nil:
		changes = append(changes, fmt.Sprintf("claude[%d].cloak: added", index))
	case o != nil && n == nil:
		changes = append(changes, fmt.Sprintf("claude[%d].cloak: removed", index))
	case o != nil && n != nil:
		if strings.TrimSpace(o.Mode) != strings.TrimSpace(n.Mode) {
			changes = append(changes, fmt.Sprintf("claude[%d].cloak.mode: %s -> %s", index, o.Mode, n.Mode))
		}
		if o.StrictMode != n.StrictMode {
			changes = append(changes, fmt.Sprintf("claude[%d].cloak.strict-mode: %t -> %t", index, o.StrictMode, n.StrictMode))
		}
		if !reflect.DeepEqual(o.SensitiveWords, n.SensitiveWords) {
			changes = append(changes, fmt.Sprintf("claude[%d].cloak.sensitive-words: %d -> %d", index, len(o.SensitiveWords), len(n.SensitiveWords)))
		}
		if !equalStringMap(o.SensitiveWordsReplace, n.SensitiveWordsReplace) {
			changes = append(changes, fmt.Sprintf("claude[%d].cloak.sensitive-words-replace: %d -> %d", index, len(o.SensitiveWordsReplace), len(n.SensitiveWordsReplace)))
		}
		if !slices.Equal(o.SensitivePatternsReplace, n.SensitivePatternsReplace) {
			changes = append(changes, fmt.Sprintf("claude[%d].cloak.sensitive-patterns-replace: %d -> %d", index, len(o.SensitivePatternsReplace), len(n.SensitivePatternsReplace)))
		}
		if !reflect.DeepEqual(o.CacheUserID, n.CacheUserID) {
			changes = append(changes, fmt.Sprintf("claude[%d].cloak.cache-user-id: %s -> %s", index, formatOptionalBool(o.CacheUserID), formatOptionalBool(n.CacheUserID)))
		}
	}
	return changes
}
