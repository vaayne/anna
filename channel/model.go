package channel

// ModelOption represents a selectable provider/model combination.
type ModelOption struct {
	Provider string
	Model    string
}

// ModelSwitchFunc switches the active model in the pool.
// It rebuilds the runner factory for the given provider/model pair.
type ModelSwitchFunc func(provider, model string) error
