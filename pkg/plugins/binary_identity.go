package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// BinaryArtifactIdentity returns the content identity used by release-built
// native CLI artifacts. Resource ownership is deliberately absent: the same
// executable can be reused by different plugin configs when its effective
// install inputs are identical.
func BinaryArtifactIdentity(spec PluginBinarySpec) (string, error) {
	version := spec.Version
	if version == "" {
		version = "latest"
	}
	options := spec.Options
	if options == nil {
		options = map[string]any{}
	}
	payload, err := json.Marshal(struct {
		Name    string         `json:"name"`
		Tool    string         `json:"tool"`
		Version string         `json:"version"`
		Options map[string]any `json:"options"`
	}{
		Name: spec.Name, Tool: spec.Tool, Version: version, Options: options,
	})
	if err != nil {
		return "", fmt.Errorf("encode binary artifact identity: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}
