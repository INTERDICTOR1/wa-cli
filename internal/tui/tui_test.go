package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codebyoketch/wa-cli/internal/chatstore"
)

// visibleLines mimics Bubble Tea's standard renderer: a frame taller than
// the terminal keeps only its bottom `height` lines.
func visibleLines(view string, height int) []string {
	lines := strings.Split(view, "\n")
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return lines
}

func TestSelectionVisibleWithManyChats(t *testing.T) {
	m := newModel(nil, nil, nil, nil, nil)
	var updated tea.Model = m
	updated, _ = updated.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	chats := make([]chatstore.Chat, 921)
	for i := range chats {
		chats[i] = chatstore.Chat{JID: fmt.Sprintf("%d@s.whatsapp.net", i), Name: fmt.Sprintf("chat %d", i), LastMessageAt: int64(10000 - i)}
		if i%3 == 0 {
			chats[i].Name = fmt.Sprintf("chat %d with a very long name 🎉🎉🎉", i)
			chats[i].UnreadCount = 123
		}
	}
	updated, _ = updated.Update(chatsLoadedMsg{chats: chats})

	for i := 1; i <= 40; i++ {
		updated, _ = updated.Update(tea.KeyMsg{Type: tea.KeyDown})
		view := updated.View()
		if n := strings.Count(view, "\n") + 1; n > 30 {
			t.Fatalf("after %d downs: frame is %d lines tall in a 30-line terminal", i, n)
		}
		want := fmt.Sprintf("› chat %d", i)
		if !strings.Contains(strings.Join(visibleLines(view, 30), "\n"), want) {
			t.Fatalf("after %d downs: selected chat %q is not on screen", i, want)
		}
	}
}
