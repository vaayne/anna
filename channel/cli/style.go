package cli

import "github.com/charmbracelet/lipgloss"

// Color palette — two accents + neutral chrome tones.
const (
	colorChatText    = lipgloss.Color("#d4d4d4") // 252 — brightest, main content
	colorUserAccent  = lipgloss.Color("#5fafaf") // 73  — teal
	colorAgentAccent = lipgloss.Color("#af87d7") // 140 — purple
	colorChrome      = lipgloss.Color("#585858") // 240 — borders, separators
	colorDimMeta     = lipgloss.Color("#4e4e4e") // 239 — footer hints, model info
	colorPlaceholder = lipgloss.Color("#6c6c6c") // 242 — input placeholder
	colorStatus      = lipgloss.Color("#d7af5f") // 179 — warm yellow, status text
	colorError       = lipgloss.Color("#d75f5f") // 167 — softer red
	colorSystem      = lipgloss.Color("#6c6c6c") // 242 — system messages
)

var (
	// Speaker labels — medium brightness, not the brightest element.
	userStyle = lipgloss.NewStyle().
			Foreground(colorUserAccent).
			Bold(true)

	agentStyle = lipgloss.NewStyle().
			Foreground(colorAgentAccent).
			Bold(true)

	// Message left borders for turn grouping.
	userBorderStyle = lipgloss.NewStyle().
			Border(lipgloss.Border{Left: "│"}, false, false, false, true).
			BorderForeground(colorUserAccent).
			PaddingLeft(1)

	agentBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.Border{Left: "│"}, false, false, false, true).
				BorderForeground(colorAgentAccent).
				PaddingLeft(1)

	// Chat text — explicit bright foreground, not terminal default.
	chatTextStyle = lipgloss.NewStyle().
			Foreground(colorChatText)

	systemStyle = lipgloss.NewStyle().
			Foreground(colorSystem).
			Italic(true)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorError)

	helpStyle = lipgloss.NewStyle().
			Foreground(colorDimMeta)

	helpAccentStyle = lipgloss.NewStyle().
			Foreground(colorChrome)

	statusStyle = lipgloss.NewStyle().
			Foreground(colorStatus).
			Italic(true)

	// Header
	titleStyle = lipgloss.NewStyle().
			Foreground(colorAgentAccent).
			Bold(true)

	modelInfoStyle = lipgloss.NewStyle().
			Foreground(colorDimMeta)

	// Composer — no box border
	inputSeparator = lipgloss.NewStyle().
			Foreground(colorChrome)

	inputPromptStyle = lipgloss.NewStyle().
				Foreground(colorChrome)

	// Tool use
	toolDoneStyle = lipgloss.NewStyle().
			Foreground(colorDimMeta)

	toolErrorStyle = lipgloss.NewStyle().
			Foreground(colorError).
			Faint(true)

	// Model picker
	modelCursorStyle = lipgloss.NewStyle().
				Foreground(colorAgentAccent).
				Bold(true)

	modelActiveStyle = lipgloss.NewStyle().
				Foreground(colorStatus).
				Bold(true)

	modelItemStyle = lipgloss.NewStyle().
			Foreground(colorChatText)

	modelHeaderStyle = lipgloss.NewStyle().
				Foreground(colorUserAccent).
				Bold(true)

	// Command completion
	completionItemStyle = lipgloss.NewStyle().
				Foreground(colorChrome)

	completionSelectedStyle = lipgloss.NewStyle().
				Foreground(colorAgentAccent).
				Bold(true)

	completionDescStyle = lipgloss.NewStyle().
				Foreground(colorDimMeta)

	completionSelectedDescStyle = lipgloss.NewStyle().
					Foreground(colorChrome).
					Italic(true)

	// Model picker filter
	filterLabelStyle = lipgloss.NewStyle().
				Foreground(colorUserAccent)

	filterTextStyle = lipgloss.NewStyle().
			Foreground(colorChatText).
			Bold(true)
)
