package termimg

import "testing"

func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Protocol
	}{
		{"nothing set", nil, Blocks},
		{"plain xterm", map[string]string{"TERM": "xterm-256color"}, Blocks},
		{"Windows Terminal", map[string]string{"WT_SESSION": "c0ffee00-1234-5678-9abc-def012345678"}, Sixel},
		{"Windows Terminal running WSL", map[string]string{"WT_SESSION": "x", "TERM": "xterm-256color"}, Sixel},
		{"old Windows console", map[string]string{"TERM": ""}, Blocks},
		{"iTerm2", map[string]string{"TERM_PROGRAM": "iTerm.app", "TERM": "xterm-256color"}, ITerm},
		{"iTerm2 through ssh", map[string]string{"LC_TERMINAL": "iTerm2", "TERM": "xterm-256color"}, ITerm},
		{"WezTerm", map[string]string{"TERM_PROGRAM": "WezTerm", "TERM": "xterm-256color"}, ITerm},
		{"Kitty by window id", map[string]string{"KITTY_WINDOW_ID": "1", "TERM": "xterm-256color"}, Kitty},
		{"Kitty by TERM", map[string]string{"TERM": "xterm-kitty"}, Kitty},
		{"Ghostty", map[string]string{"TERM_PROGRAM": "ghostty", "TERM": "xterm-ghostty"}, Kitty},
		{"xterm built with Sixel", map[string]string{"TERM": "xterm-sixel"}, Sixel},
		{"a TERM that mentions sixel", map[string]string{"TERM": "yaft-256color-sixel"}, Sixel},
		{"mlterm by TERM_PROGRAM", map[string]string{"TERM_PROGRAM": "mlterm"}, Sixel},
		{"mlterm by TERM", map[string]string{"TERM": "mlterm-256color"}, Sixel},
		{"foot", map[string]string{"TERM": "foot"}, Sixel},
		{"foot-extra", map[string]string{"TERM": "foot-extra"}, Sixel},
		{"foot by TERM_PROGRAM", map[string]string{"TERM_PROGRAM": "foot"}, Sixel},
		{"Apple Terminal", map[string]string{"TERM_PROGRAM": "Apple_Terminal", "TERM": "xterm-256color"}, Blocks},
		{"VS Code", map[string]string{"TERM_PROGRAM": "vscode", "TERM": "xterm-256color"}, Blocks},
		{"tmux", map[string]string{"TERM": "tmux-256color", "TMUX": "/tmp/tmux-1000/default,1,0"}, Blocks},
		// The order of the checks, when several match.
		{"Windows Terminal wins over iTerm", map[string]string{"WT_SESSION": "x", "TERM_PROGRAM": "iTerm.app"}, Sixel},
		{"iTerm wins over Kitty", map[string]string{"LC_TERMINAL": "iTerm2", "KITTY_WINDOW_ID": "1"}, ITerm},
		{"Kitty wins over a sixel TERM", map[string]string{"KITTY_WINDOW_ID": "1", "TERM": "xterm-sixel"}, Kitty},
		// Exact names only.
		{"case matters", map[string]string{"TERM_PROGRAM": "wezterm"}, Blocks},
		{"kitty must be the whole TERM", map[string]string{"TERM": "xterm-kitty-like"}, Blocks},
	}
	for _, tt := range tests {
		got := Detect(func(key string) string { return tt.env[key] })
		if got != tt.want {
			t.Errorf("%s: Detect = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestProtocolNames(t *testing.T) {
	names := map[Protocol]string{Blocks: "blocks", Sixel: "sixel", ITerm: "iterm", Kitty: "kitty"}
	for p, name := range names {
		if got := p.String(); got != name {
			t.Errorf("Protocol(%d).String() = %q, want %q", int(p), got, name)
		}
		if got, ok := ParseProtocol(name); !ok || got != p {
			t.Errorf("ParseProtocol(%q) = %v, %v; want %v, true", name, got, ok, p)
		}
	}
	for in, want := range map[string]Protocol{"SIXEL": Sixel, " Kitty\n": Kitty, "iTerm": ITerm, "Blocks": Blocks} {
		if got, ok := ParseProtocol(in); !ok || got != want {
			t.Errorf("ParseProtocol(%q) = %v, %v; want %v, true", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "auto", "Auto", "none", "sixels", "iterm2", "half-blocks", "0", "kitty graphics"} {
		if got, ok := ParseProtocol(in); ok || got != Blocks {
			t.Errorf("ParseProtocol(%q) = %v, %v; want blocks, false", in, got, ok)
		}
	}
	// A number that isn't a protocol still prints as something sensible.
	for _, p := range []Protocol{-1, 4, 99} {
		if got := p.String(); got != "blocks" {
			t.Errorf("Protocol(%d).String() = %q, want blocks", int(p), got)
		}
	}
	// The zero value is the fallback that works everywhere.
	var zero Protocol
	if zero != Blocks {
		t.Error("the zero Protocol is not Blocks")
	}
}

func TestCellPixels(t *testing.T) {
	for _, p := range []Protocol{Blocks, Sixel, ITerm, Kitty} {
		if w, h := CellPixels(p); w != 10 || h != 20 {
			t.Errorf("CellPixels(%v) = %d, %d; want 10, 20", p, w, h)
		}
	}
}
