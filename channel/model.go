package channel

// ModelOption represents a selectable provider/model combination.
type ModelOption struct {
	Provider string
	Model    string
}

// ModelListFunc returns the current list of available models.
// Called on demand so callers always see the latest cached models.
type ModelListFunc func() []ModelOption

// ModelSwitchFunc switches the active model in the pool.
// It rebuilds the runner factory for the given provider/model pair.
type ModelSwitchFunc func(provider, model string) error
