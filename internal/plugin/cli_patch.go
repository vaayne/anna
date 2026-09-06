package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// applyCLIWriteOnlyPatch turns the small public CLI edit maps into a normal
// payload overlay. The definition and the target config are the only inputs;
// resolving another scope here would let a caller edit data it cannot see.
func applyCLIWriteOnlyPatch(def Definition, current json.RawMessage, patch ConfigPatch, _ bool) (json.RawMessage, error) {
	if def.Backend != BackendCLI {
		return nil, fmt.Errorf("%w: CLI write-only fields require a CLI backend", ErrInvalidConfig)
	}
	var shipped map[string]json.RawMessage
	if err := decodeJSONObject(def.Spec, &shipped); err != nil {
		return nil, fmt.Errorf("%w: definition spec must be an object", ErrInvalidConfig)
	}
	var owned map[string]json.RawMessage
	if len(current) != 0 {
		if err := decodeJSONObject(current, &owned); err != nil {
			return nil, fmt.Errorf("%w: config payload must be an object", ErrInvalidConfig)
		}
	} else {
		owned = map[string]json.RawMessage{}
	}

	result := map[string]json.RawMessage{}
	if patch.BinaryVersionsSet && len(patch.BinaryVersions) > 0 {
		binaries, err := materializeBinaries(shipped["binaries"], owned["binaries"])
		if err != nil {
			return nil, err
		}
		byName := make(map[string]map[string]json.RawMessage, len(binaries))
		for i := range binaries {
			name, err := resourceName(binaries[i], "binary", i)
			if err != nil {
				return nil, err
			}
			byName[name] = binaries[i]
		}
		for name, version := range patch.BinaryVersions {
			if strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" {
				return nil, fmt.Errorf("%w: binary name and version are required", ErrInvalidConfig)
			}
			binary, ok := byName[name]
			if !ok {
				return nil, fmt.Errorf("%w: unknown binary %q", ErrInvalidConfig, name)
			}
			encoded, err := json.Marshal(version)
			if err != nil {
				return nil, fmt.Errorf("%w: binary version %q", ErrInvalidConfig, name)
			}
			binary["version"] = encoded
		}
		encoded, err := json.Marshal(binaries)
		if err != nil {
			return nil, fmt.Errorf("%w: binaries", ErrInvalidConfig)
		}
		result["binaries"] = encoded
	}
	return json.Marshal(result)
}

func decodeJSONObject(raw json.RawMessage, dst *map[string]json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("empty object")
	}
	if err := json.Unmarshal(trimmed, dst); err != nil || *dst == nil {
		return fmt.Errorf("invalid object")
	}
	return nil
}

func materializeBinaries(shipped, owned json.RawMessage) ([]map[string]json.RawMessage, error) {
	base, err := decodeResourceArray(shipped, "definition binaries")
	if err != nil {
		return nil, err
	}
	return overlayResources(base, owned, "binary", func(item map[string]json.RawMessage) (string, error) {
		return resourceName(item, "binary", 0)
	})
}

func decodeResourceArray(raw json.RawMessage, field string) ([]map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: definition has no %s", ErrInvalidConfig, field)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, fmt.Errorf("%w: %s must be an array", ErrInvalidConfig, field)
	}
	return items, nil
}

func overlayResources(base []map[string]json.RawMessage, owned json.RawMessage, kind string, nameOf func(map[string]json.RawMessage) (string, error)) ([]map[string]json.RawMessage, error) {
	byName := make(map[string]int, len(base))
	for i, item := range base {
		name, err := nameOf(item)
		if err != nil {
			return nil, err
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("%w: duplicate %s %q", ErrInvalidConfig, kind, name)
		}
		byName[name] = i
	}
	if len(owned) == 0 || bytes.Equal(bytes.TrimSpace(owned), []byte("null")) {
		return base, nil
	}
	var overrides []map[string]json.RawMessage
	if err := json.Unmarshal(owned, &overrides); err != nil || overrides == nil {
		return nil, fmt.Errorf("%w: %s config must be an array", ErrInvalidConfig, kind)
	}
	seen := make(map[string]struct{}, len(overrides))
	for i, item := range overrides {
		name, err := resourceName(item, kind, i)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("%w: duplicate %s %q", ErrInvalidConfig, kind, name)
		}
		seen[name] = struct{}{}
		idx, exists := byName[name]
		if !exists {
			return nil, fmt.Errorf("%w: config contains unknown %s %q", ErrInvalidConfig, kind, name)
		}
		maps.Copy(base[idx], item)
	}
	return base, nil
}

func resourceName(item map[string]json.RawMessage, kind string, index int) (string, error) {
	raw, ok := item["name"]
	if !ok {
		return "", fmt.Errorf("%w: %s[%d] has no name", ErrInvalidConfig, kind, index)
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: %s[%d] has invalid name", ErrInvalidConfig, kind, index)
	}
	return name, nil
}
