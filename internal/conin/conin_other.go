//go:build !windows

package conin

import tea "github.com/charmbracelet/bubbletea"

// Reader is a running console input reader. On this operating system there
// is never one: Start always returns nil, and the methods below accept that.
type Reader struct{}

// Start begins reading the Windows console's input and forwards Bubble Tea
// messages to send. It returns (nil, nil) when unsupported — not Windows, or
// stdin is not a console (piped/redirected) — in which case the caller lets
// Bubble Tea read input itself.
//
// This is the non-Windows version, so it always returns (nil, nil). Bubble
// Tea's own reader handles bracketed paste correctly here.
func Start(send func(tea.Msg), opts Options) (*Reader, error) {
	return nil, nil
}

// Stop restores the console's original input mode and ends the read loop.
// Safe to call more than once and on a nil *Reader. Here it does nothing.
func (r *Reader) Stop() {}

// Divert routes every message to fn first while fn is non-nil; a message is
// dropped (not passed to send) when fn returns true. Divert(nil) clears it.
// Safe on a nil *Reader. Here it does nothing.
func (r *Reader) Divert(fn func(tea.Msg) bool) {}
