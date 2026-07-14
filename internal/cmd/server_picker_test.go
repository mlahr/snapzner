package cmd

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

func TestServerPickerKeyboardSelection(t *testing.T) {
	state := pickerState{
		servers:  []*hcloud.Server{{ID: 1, Name: "db"}, {ID: 2, Name: "web"}},
		matched:  map[int64]bool{1: true},
		selected: map[int64]bool{1: true},
	}
	model := newServerPickerModel("prod", state)
	model = updateServerPicker(t, model, tea.KeyMsg{Type: tea.KeyDown})
	model = updateServerPicker(t, model, tea.KeyMsg{Type: tea.KeySpace})
	if !model.state.selected[1] || !model.state.selected[2] {
		t.Fatalf("selection = %#v", model.state.selected)
	}
	model = updateServerPicker(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	if model.state.selected[1] || model.state.selected[2] {
		t.Fatalf("none selection = %#v", model.state.selected)
	}
	model = updateServerPicker(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !model.state.selected[1] || !model.state.selected[2] {
		t.Fatalf("all selection = %#v", model.state.selected)
	}
	model = updateServerPicker(t, model, tea.KeyMsg{Type: tea.KeyEnter})
	if !model.accepted || model.cancelled {
		t.Fatalf("unexpected completion state: accepted=%t cancelled=%t", model.accepted, model.cancelled)
	}
}

func TestServerPickerCancelAndView(t *testing.T) {
	state := pickerState{
		servers:  []*hcloud.Server{{ID: 1, Name: "database"}},
		matched:  map[int64]bool{1: true},
		selected: map[int64]bool{1: true},
	}
	model := newServerPickerModel("production", state)
	view := model.View()
	for _, want := range []string{"project production", "database", "id=1", "selector match", "1 of 1 selected"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view does not contain %q:\n%s", want, view)
		}
	}
	model = updateServerPicker(t, model, tea.KeyMsg{Type: tea.KeyEsc})
	if !model.cancelled || model.accepted {
		t.Fatalf("unexpected completion state: accepted=%t cancelled=%t", model.accepted, model.cancelled)
	}
}

func updateServerPicker(t *testing.T, model serverPickerModel, message tea.Msg) serverPickerModel {
	t.Helper()
	next, _ := model.Update(message)
	updated, ok := next.(serverPickerModel)
	if !ok {
		t.Fatalf("model type = %T", next)
	}
	return updated
}
