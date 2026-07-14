package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

var errServerPickerCancelled = errors.New("server selection cancelled")

type serverPickerModel struct {
	project               string
	state                 pickerState
	cursor, width, height int
	accepted, cancelled   bool
}

func newServerPickerModel(project string, state pickerState) serverPickerModel {
	state.selected = cloneSelection(state.selected)
	return serverPickerModel{project: project, state: state, width: 80, height: 24}
}

func (m serverPickerModel) Init() tea.Cmd { return nil }

func (m serverPickerModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "esc", "q":
			m.cancelled = true
			return m, tea.Quit
		case "enter":
			m.accepted = true
			return m, tea.Quit
		case "up", "k", "ctrl+p":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j", "ctrl+n":
			if m.cursor < len(m.state.servers)-1 {
				m.cursor++
			}
		case "home", "g":
			m.cursor = 0
		case "end", "G":
			if len(m.state.servers) > 0 {
				m.cursor = len(m.state.servers) - 1
			}
		case "pgup":
			m.cursor -= m.pageSize()
			if m.cursor < 0 {
				m.cursor = 0
			}
		case "pgdown":
			if len(m.state.servers) > 0 {
				m.cursor += m.pageSize()
				if m.cursor >= len(m.state.servers) {
					m.cursor = len(m.state.servers) - 1
				}
			}
		case " ", "x":
			if len(m.state.servers) > 0 {
				id := m.state.servers[m.cursor].ID
				m.state.selected[id] = !m.state.selected[id]
			}
		case "a":
			for _, server := range m.state.servers {
				m.state.selected[server.ID] = true
			}
		case "n":
			for _, server := range m.state.servers {
				m.state.selected[server.ID] = false
			}
		}
	}
	return m, nil
}

func (m serverPickerModel) View() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Select servers for project %s\n\n", m.project)
	start, end := m.visibleRange()
	if start > 0 {
		b.WriteString("    ↑ more\n")
	}
	for i := start; i < end; i++ {
		server := m.state.servers[i]
		cursor := "  "
		if i == m.cursor {
			cursor = "> "
		}
		mark := " "
		if m.state.selected[server.ID] {
			mark = "x"
		}
		suffix := ""
		if m.state.matched[server.ID] {
			suffix = "  selector match"
		}
		line := fmt.Sprintf("%s[%s] %-32s id=%d%s", cursor, mark, server.Name, server.ID, suffix)
		b.WriteString(truncateTerminalLine(line, m.width))
		b.WriteByte('\n')
	}
	if end < len(m.state.servers) {
		b.WriteString("    ↓ more\n")
	}
	fmt.Fprintf(&b, "\n%d of %d selected\n", m.state.count(), len(m.state.servers))
	b.WriteString("↑/↓ move  space toggle  a all  n none  enter confirm  esc cancel\n")
	return b.String()
}

func (m serverPickerModel) pageSize() int {
	size := m.height - 8
	if size < 1 {
		return 1
	}
	return size
}

func (m serverPickerModel) visibleRange() (int, int) {
	size := m.pageSize()
	start := m.cursor - size/2
	if start < 0 {
		start = 0
	}
	maxStart := len(m.state.servers) - size
	if maxStart < 0 {
		maxStart = 0
	}
	if start > maxStart {
		start = maxStart
	}
	end := start + size
	if end > len(m.state.servers) {
		end = len(m.state.servers)
	}
	return start, end
}

func runServerPicker(ctx context.Context, output io.Writer, project string, state pickerState) (map[int64]bool, error) {
	program := tea.NewProgram(newServerPickerModel(project, state), tea.WithContext(ctx), tea.WithInput(os.Stdin), tea.WithOutput(output), tea.WithAltScreen())
	result, err := program.Run()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	model, ok := result.(serverPickerModel)
	if !ok {
		return nil, fmt.Errorf("unexpected server picker result")
	}
	if model.cancelled || !model.accepted {
		return nil, errServerPickerCancelled
	}
	return cloneSelection(model.state.selected), nil
}

func cloneSelection(source map[int64]bool) map[int64]bool {
	clone := make(map[int64]bool, len(source))
	for id, selected := range source {
		clone[id] = selected
	}
	return clone
}

func truncateTerminalLine(value string, width int) string {
	if width < 2 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	return string(runes[:width-1]) + "…"
}
