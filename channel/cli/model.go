package cli

import (
	"fmt"
	"strings"
)

// modelOption represents a selectable provider/model combination.
type modelOption struct {
	provider string
	model    string
}

func filterModels(models []modelOption, query string) []modelOption {
	if query == "" {
		return models
	}
	query = strings.ToLower(query)
	var result []modelOption
	for _, m := range models {
		label := strings.ToLower(m.provider + "/" + m.model)
		if strings.Contains(label, query) {
			result = append(result, m)
		}
	}
	return result
}

// renderModelPicker renders the model selection list with optional filter.
func renderModelPicker(models []modelOption, cursor int, activeProvider, activeModel, filter string) string {
	var sb strings.Builder
	sb.WriteString(modelHeaderStyle.Render("Select Model") + "\n")

	if filter != "" {
		sb.WriteString(filterLabelStyle.Render("Filter: ") + filterTextStyle.Render(filter) + "\n")
	} else {
		sb.WriteString(helpStyle.Render("Type to filter · ↑/↓ navigate · Enter select · Esc cancel") + "\n")
	}
	sb.WriteString("\n")

	if len(models) == 0 {
		sb.WriteString(helpStyle.Render("  No matching models") + "\n")
		return sb.String()
	}

	for i, m := range models {
		label := fmt.Sprintf("%s/%s", m.provider, m.model)
		isActive := m.provider == activeProvider && m.model == activeModel

		prefix := "  "
		if i == cursor {
			prefix = "> "
		}

		var line string
		switch {
		case i == cursor && isActive:
			line = modelCursorStyle.Render(prefix) + modelActiveStyle.Render(label+" (current)")
		case i == cursor:
			line = modelCursorStyle.Render(prefix+label)
		case isActive:
			line = modelActiveStyle.Render(prefix+label+" (current)")
		default:
			line = modelItemStyle.Render(prefix+label)
		}

		sb.WriteString(line + "\n")
	}

	return sb.String()
}
