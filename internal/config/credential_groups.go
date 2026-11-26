package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"sort"

	"gopkg.in/yaml.v3"
)

// credentialGroupScopes follows expandV8Groups order before sanitization. A source
// never contains a sibling key or a saved copy of an API key.
func credentialGroupScopes(families map[string]any) (map[string][]map[string]any, error) {
	if len(families) == 0 {
		return nil, nil
	}
	result := make(map[string][]map[string]any)
	for _, family := range v8KeyFamilies {
		groups, ok := families[family.current].([]any)
		if !ok {
			continue
		}
		occurrences := make(map[string]int)
		for _, raw := range groups {
			group, okGroup := raw.(map[string]any)
			if !okGroup {
				return nil, fmt.Errorf("invalid %s credential group", family.current)
			}
			defaults := maps.Clone(group)
			delete(defaults, "keys")
			signature, errMarshal := json.Marshal(defaults)
			if errMarshal != nil {
				return nil, errMarshal
			}
			occurrence := occurrences[string(signature)]
			occurrences[string(signature)]++
			// Separate identical submitted groups without tying identity to unrelated group ordering.
			scope := map[string]any{"source": fmt.Sprintf("%x", sha256.Sum256(signature)), "partition": float64(occurrence)}
			if family.current == "openai-compatibility" {
				result[family.old] = append(result[family.old], scope)
				continue
			}
			scope["group"] = defaults
			keys, _ := group["keys"].([]any)
			for _, item := range keys {
				key, _ := item.(map[string]any)
				member := maps.Clone(scope)
				fields := make(map[string]any)
				for name, value := range key {
					if name == "connection-pool" {
						member[name] = value
					} else if sharedKeyFields[name] {
						fields[name] = value != nil
					}
				}
				member["fields"] = fields
				result[family.old] = append(result[family.old], member)
			}
		}
	}
	return result, nil
}

// SetV8CredentialGroups moves client access keys out of the legacy api-keys
// slot before assigning the V8 provider map. Other families may still be flat.
func SetV8CredentialGroups(root, groups map[string]any) {
	if len(groups) == 0 {
		return
	}
	if clientKeys, exists := root["api-keys"]; exists {
		access, _ := root["access"].(map[string]any)
		access = maps.Clone(access)
		if access == nil {
			access = make(map[string]any)
		}
		access["api-keys"] = clientKeys
		root["access"] = access
	}
	root["api-keys"] = groups
	for _, family := range v8KeyFamilies {
		if _, ok := groups[family.current]; ok {
			delete(root, family.old)
		}
	}
}

// RestoreV8CredentialGroups reassembles live credentials. Older rows without
// source metadata keep their existing legacy grouping.
func RestoreV8CredentialGroups(root map[string]any, scopes map[string]map[string]any) error {
	if len(scopes) == 0 {
		return nil
	}
	families := make(map[string]any)
	for _, family := range v8KeyFamilies {
		entries, exists := root[family.old]
		if !exists {
			continue
		}
		var flat yaml.Node
		if errEncode := flat.Encode(entries); errEncode != nil {
			return errEncode
		}
		var rows []map[string]any
		if errDecode := groupLegacyKeys(&flat, family.current).Decode(&rows); errDecode != nil {
			return errDecode
		}
		restored, changed, errRestore := restoreCredentialFamily(rows, scopes, family.current == "openai-compatibility")
		if errRestore != nil {
			return errRestore
		}
		if changed {
			families[family.current] = restored
		}
	}
	SetV8CredentialGroups(root, families)
	return nil
}

func restoreCredentialFamily(rows []map[string]any, scopes map[string]map[string]any, compatibility bool) ([]map[string]any, bool, error) {
	var groups []map[string]any
	type groupID struct {
		source    string
		defaults  string
		partition int
	}
	byID := make(map[groupID]map[string]any)
	var order []groupID
	changed := false
	for _, row := range rows {
		keys, _ := row["keys"].([]any)
		if len(keys) == 0 {
			groups = append(groups, row)
			continue
		}
		var legacy []any
		for _, item := range keys {
			key, _ := item.(map[string]any)
			id, _ := key["id"].(string)
			scope := scopes[id]
			if scope == nil {
				legacy = append(legacy, key)
				continue
			}
			group := maps.Clone(row)
			delete(group, "keys")
			if !compatibility {
				defaults, ok := scope["group"].(map[string]any)
				if !ok {
					return nil, false, fmt.Errorf("missing V8 group for credential %q", id)
				}
				group, key = restoreNativeGroup(group, key, defaults, scope)
			}
			signature, errMarshal := json.Marshal(group)
			if errMarshal != nil {
				return nil, false, errMarshal
			}
			partition, _ := scope["partition"].(float64)
			// Separate original groups even if a live endpoint edit makes their current defaults identical.
			source, _ := scope["source"].(string)
			groupKey := groupID{source, string(signature), int(partition)}
			target := byID[groupKey]
			if target == nil {
				target = group
				byID[groupKey] = target
				order = append(order, groupKey)
			}
			members, _ := target["keys"].([]any)
			target["keys"] = append(members, key)
			changed = true
		}
		if len(legacy) != 0 {
			row["keys"] = legacy
			groups = append(groups, row)
		}
	}
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].defaults == order[j].defaults {
			if order[i].source != order[j].source {
				return order[i].source < order[j].source
			}
			return order[i].partition < order[j].partition
		}
		return order[i].defaults < order[j].defaults
	})
	for _, id := range order {
		groups = append(groups, byID[id])
	}
	return groups, changed, nil
}

func restoreNativeGroup(live, key, defaults, scope map[string]any) (map[string]any, map[string]any) {
	group := maps.Clone(defaults)
	fields, _ := scope["fields"].(map[string]any)
	for name := range sharedKeyFields {
		if name == "connection-pool" {
			continue
		}
		actual, exists := live[name]
		if present, override := fields[name]; override {
			if present == false && sameGroupField(actual, defaults[name]) {
				key[name] = nil
			} else if exists {
				key[name] = actual
			} else {
				// Dropping an authoritative value must not reactivate an old group default.
				delete(group, name)
			}
		} else if exists {
			if _, declared := defaults[name]; declared || actual != "" {
				group[name] = actual
			}
		} else if defaults[name] != nil {
			delete(group, name)
		}
	}
	if base, ok := live["base-url"]; ok {
		group["base-url"] = base
	} else {
		delete(group, "base-url")
	}
	if pool, ok := scope["connection-pool"]; ok {
		key["connection-pool"] = pool
	}
	return group, key
}

// The stored scope uses JSON numbers whereas the live YAML tree uses Go integers.
func sameGroupField(left, right any) bool {
	a, errLeft := json.Marshal(left)
	b, errRight := json.Marshal(right)
	return errLeft == nil && errRight == nil && bytes.Equal(a, b)
}
