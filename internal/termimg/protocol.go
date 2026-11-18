package termimg

import "strings"

// Protocol is a way to put pixels on a terminal.
type Protocol int

const (
	Blocks Protocol = iota // half blocks: works everywhere with 24-bit colour
	Sixel                  // real pixels, up to 256 colours: WriteSixel
	ITerm                  // the terminal decodes the file itself: WriteITerm
	Kitty                  // real pixels in full colour: WriteKitty
)

var protocolNames = [...]string{Blocks: "blocks", Sixel: "sixel", ITerm: "iterm", Kitty: "kitty"}

// String returns "blocks", "sixel", "iterm" or "kitty".
func (p Protocol) String() string {
	if p < 0 || int(p) >= len(protocolNames) {
		return "blocks"
	}
	return protocolNames[p]
}

// ParseProtocol is the inverse of String, ignoring case and surrounding
// space. Anything else, including "auto" and "", gives (Blocks, false): the
// caller should then ask Detect.
func ParseProtocol(s string) (Protocol, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	for p, name := range protocolNames {
		if s == name {
			return Protocol(p), true
		}
	}
	return Blocks, false
}

// Detect guesses the best protocol from the environment (pass os.Getenv).
// Terminals don't announce what they can draw in any common way, so this
// goes by the variables they are known to set:
//
//   - WT_SESSION: Windows Terminal, which draws Sixel (since 1.22)
//   - TERM_PROGRAM=iTerm.app or WezTerm, or LC_TERMINAL=iTerm2 (which
//     survives ssh): the iTerm protocol
//   - KITTY_WINDOW_ID, TERM=xterm-kitty or TERM_PROGRAM=ghostty: Kitty
//   - a TERM that mentions sixel, or mlterm or foot by TERM_PROGRAM or
//     TERM: Sixel
//
// Everything else gets half blocks, which only need 24-bit colour.
func Detect(getenv func(string) string) Protocol {
	term, program := getenv("TERM"), getenv("TERM_PROGRAM")
	switch {
	case getenv("WT_SESSION") != "":
		return Sixel
	case program == "iTerm.app", program == "WezTerm", getenv("LC_TERMINAL") == "iTerm2":
		return ITerm
	case getenv("KITTY_WINDOW_ID") != "", term == "xterm-kitty", program == "ghostty":
		return Kitty
	case strings.Contains(term, "sixel"),
		program == "mlterm", program == "foot",
		strings.HasPrefix(term, "foot"), strings.HasPrefix(term, "mlterm"):
		return Sixel
	}
	return Blocks
}

// CellPixels is the pixel size of one terminal cell to assume for a protocol
// when the terminal can't be asked: 10x20. That is what Windows Terminal
// uses for Sixel whatever the font size (it scales the picture, as the VT340
// it imitates had 10x20 cells), and a fine default elsewhere.
func CellPixels(p Protocol) (w, h int) {
	return 10, 20
}
