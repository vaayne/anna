package cli

import "github.com/charmbracelet/lipgloss"

var (
	userStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("6")).
			Bold(true)

	agentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("5")).
			Bold(true)

	systemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8")).
			Italic(true)

	errorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3")).
			Italic(true)

	titleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("5")).
			Bold(true)

	modelInfoStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	separatorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("8"))

	viewportBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("8"))

	modelCursorStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("5")).
				Bold(true)

	modelActiveStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("3")).
				Bold(true)

	modelItemStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("7"))

	modelHeaderStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("6")).
				Bold(true)

	completionItemStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))

	completionSelectedStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("5")).
				Bold(true)

	completionDescStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("8"))

	completionSelectedDescStyle = lipgloss.NewStyle().
					Foreground(lipgloss.Color("8")).
					Italic(true)

	filterLabelStyle = lipgloss.NewStyle().
				Foreground(lipgloss.Color("6"))

	filterTextStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("7")).
			Bold(true)

	toolUseStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("3")).
			Italic(true)

	toolDoneStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("2")).
			Italic(true)

	toolErrorStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("1")).
			Italic(true)
)
