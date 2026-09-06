package plugin

import (
	"fmt"
	"slices"
)

// Catalog is the single in-process definition index used by reconciliation and
// service adapters. Registration rejects duplicate IDs; configuration owners reserve namespaces.
type Catalog struct {
	byID map[string]Definition
}

func NewCatalog() *Catalog {
	return &Catalog{byID: make(map[string]Definition)}
}

func (c *Catalog) Register(def Definition) error {
	if c == nil {
		return fmt.Errorf("plugin: nil catalog")
	}
	if err := def.Validate(); err != nil {
		return err
	}
	if existing, ok := c.byID[def.ID]; ok {
		if existing.Namespace == def.Namespace {
			return fmt.Errorf("plugin: duplicate definition %q", def.ID)
		}
		return fmt.Errorf("plugin: definition %q changes namespace", def.ID)
	}
	c.byID[def.ID] = cloneDefinition(def)
	return nil
}

func (c *Catalog) Get(id string) (Definition, bool) {
	if c == nil {
		return Definition{}, false
	}
	def, ok := c.byID[id]
	return cloneDefinition(def), ok
}

func (c *Catalog) Definitions() []Definition {
	if c == nil {
		return nil
	}
	defs := make([]Definition, 0, len(c.byID))
	for _, def := range c.byID {
		defs = append(defs, cloneDefinition(def))
	}
	slices.SortFunc(defs, func(a, b Definition) int {
		if a.Namespace < b.Namespace {
			return -1
		}
		if a.Namespace > b.Namespace {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return defs
}
