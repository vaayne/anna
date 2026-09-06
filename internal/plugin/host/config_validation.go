package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/CherryHQ/stella/internal/plugin"
)

// ValidatePayload keeps the compiled backend configuration separate from its
// domain resources. Channel accounts and email credentials retain their own
// authorized stores; a plugin permission record cannot replace those secrets.
func ValidatePayload(_ context.Context, def plugin.Definition, cfg plugin.Config, _ []string) error {
	if err := def.Validate(); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if def.Source != plugin.SourceBuiltin || def.Backend != plugin.BackendGo || def.ImplementationKey != def.ID {
		return plugin.ErrInvalidDefinition
	}
	for _, raw := range []json.RawMessage{cfg.Payload, cfg.CredentialRefs} {
		if len(raw) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 0 {
			return fmt.Errorf("%w: compiled plugin configuration accepts only its enabled decision", plugin.ErrInvalidConfig)
		}
	}
	return nil
}
