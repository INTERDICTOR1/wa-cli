// Package conin reads keyboard, paste, mouse, focus and resize events straight
// from the Windows console and hands them to Bubble Tea as ordinary messages.
//
// # Why this exists
//
// Bubble Tea v1 reads the Windows console one key press at a time and turns
// each into its own tea.KeyMsg. Windows has no "this was pasted" flag: when
// the user pastes several lines, each line break shows up as an Enter key
// press that looks exactly like a typed one. A chat program that sends on
// Enter therefore sends the first line straight away and every following
// line as a separate message.
//
// This package replaces that reader. It asks the console for everything that
// is waiting in one go, and uses what a human cannot do — make many
// characters arrive in the same instant — to tell a paste from typing. A
// paste is delivered as ONE message:
//
//	tea.KeyMsg{Type: tea.KeyRunes, Runes: <all the text>, Paste: true}
//
// which is exactly how Bubble Tea reports a bracketed paste on Linux and
// macOS, so the UI needs only one code path.
//
// It also fixes a few smaller problems of the built-in reader: Shift+Enter
// and Ctrl+Enter are distinguishable from Enter (they arrive as
// tea.KeyCtrlJ), emoji arrive as one rune instead of two broken halves,
// focus changes are reported, and nothing polls on a timer.
//
// # How to use it
//
// Start the Bubble Tea program without an input (tea.WithInput(nil)) and let
// this package feed it:
//
//	p := tea.NewProgram(model, tea.WithInput(nil), ...)
//	reader, err := conin.Start(p.Send, conin.Options{})
//	defer reader.Stop()
//
// On other operating systems — and when stdin is not a console — Start
// returns (nil, nil) and the caller keeps Bubble Tea's own reader, which
// already understands bracketed paste there.
//
// # Layout of the code
//
//   - record.go     what the console gives us (keys, mouse, …) as plain Go values
//   - keys.go       one key record -> one tea.KeyMsg (the key table)
//   - translate.go  the paste detector: batches of records -> messages
//   - deliver.go    the queue between the read loop and Bubble Tea
//   - conin_windows.go  the thin layer of Windows system calls
//   - conin_other.go    the do-nothing version for every other system
//
// Everything except conin_windows.go is ordinary Go with no system calls, so
// the tests can drive the paste detector with made-up records and a made-up
// clock.
package conin

// Options configures Start.
type Options struct {
	Mouse bool // also report mouse wheel/click events
}
