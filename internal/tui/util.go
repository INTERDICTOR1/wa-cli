package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// sanitize makes text from outside safe to draw: control characters
// (including ESC, which could otherwise smuggle terminal commands into a
// chat message) are dropped, tabs become spaces, and line breaks are kept.
func sanitize(s string) string {
	clean := true
	for _, r := range s {
		if r != '\n' && (unicode.IsControl(r) || r == ' ' || r == ' ') {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case r == ' ' || r == ' ':
			return '\n'
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// oneLine is sanitize with line breaks turned into spaces.
func oneLine(s string) string {
	s = sanitize(s)
	if strings.ContainsRune(s, '\n') {
		s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
	}
	return s
}

func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// padRight pads s with spaces to exactly w columns (truncating if longer).
func padRight(s string, w int) string {
	sw := ansi.StringWidth(s)
	if sw > w {
		return ansi.Truncate(s, w, "")
	}
	return s + strings.Repeat(" ", w-sw)
}

// spread puts left and right at the two ends of a w-column line.
func spread(left, right string, w int) string {
	lw, rw := ansi.StringWidth(left), ansi.StringWidth(right)
	if lw+rw+1 > w {
		left = truncate(left, max(0, w-rw-1))
		lw = ansi.StringWidth(left)
	}
	return left + strings.Repeat(" ", max(1, w-lw-rw)) + right
}

func center(s string, w int) string {
	sw := ansi.StringWidth(s)
	if sw >= w {
		return ansi.Truncate(s, w, "")
	}
	return strings.Repeat(" ", (w-sw)/2) + s
}

func humanSize(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	case n > 0:
		return fmt.Sprintf("%d B", n)
	}
	return ""
}

func clock(sec int) string {
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// listTime is the short time shown next to a chat: the time today, the
// weekday within the last week, otherwise the date.
func listTime(ms int64, now time.Time) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).Local()
	switch {
	case sameDay(t, now):
		return t.Format("15:04")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "Yesterday"
	case now.Sub(t) < 6*24*time.Hour && now.After(t):
		return t.Format("Mon")
	}
	return t.Format("02/01/06")
}

// dayLabel names a day for the separators between messages.
func dayLabel(t, now time.Time) string {
	switch {
	case sameDay(t, now):
		return "Today"
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "Yesterday"
	case now.Sub(t) < 6*24*time.Hour && now.After(t):
		return t.Format("Monday")
	case t.Year() == now.Year():
		return t.Format("Mon 2 Jan")
	}
	return t.Format("2 Jan 2006")
}

func lastSeenText(ms int64, now time.Time) string {
	if ms <= 0 {
		return ""
	}
	t := time.UnixMilli(ms).Local()
	switch {
	case sameDay(t, now):
		return "last seen today at " + t.Format("15:04")
	case sameDay(t, now.AddDate(0, 0, -1)):
		return "last seen yesterday at " + t.Format("15:04")
	}
	return "last seen " + t.Format("2 Jan") + " at " + t.Format("15:04")
}

var urlRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// firstURL returns the first http(s) link in s, or "".
func firstURL(s string) string {
	return strings.TrimRight(urlRe.FindString(s), ".,;:!?)")
}

// asFilePath reports whether s (as pasted or dropped onto the terminal:
// possibly quoted, possibly a file:// URL) names an existing regular file.
func asFilePath(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 1024 || strings.ContainsAny(s, "\n\r") {
		return "", false
	}
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		s = s[1 : len(s)-1]
	}
	if rest, ok := strings.CutPrefix(s, "file:///"); ok {
		if runtime.GOOS == "windows" {
			s = rest
		} else {
			s = "/" + rest
		}
		s = strings.ReplaceAll(s, "%20", " ")
	}
	if !filepath.IsAbs(s) {
		return "", false
	}
	info, err := os.Stat(s)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return filepath.Clean(s), true
}

// openableExts are the attachment types handed to the system's default
// app: ones that are shown or played. "Opening" anything else can mean
// running it - a program, a script, a shortcut, an installer, a web page
// with scripts, a document with macros - and an attachment's name is the
// sender's to choose. So the list says what is allowed, not what isn't.
var openableExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
	".heic": true, ".heif": true, ".tif": true, ".tiff": true, ".avif": true,
	".mp4": true, ".m4v": true, ".mov": true, ".3gp": true, ".webm": true, ".mkv": true, ".avi": true,
	".ogg": true, ".oga": true, ".opus": true, ".mp3": true, ".m4a": true, ".aac": true,
	".amr": true, ".wav": true, ".flac": true,
	".pdf": true, ".txt": true, ".csv": true, ".rtf": true, ".epub": true,
	".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".odt": true, ".ods": true, ".odp": true,
}

// openable reports whether a downloaded attachment may be handed to the
// system's default app.
func openable(path string) bool {
	return openableExts[strings.ToLower(filepath.Ext(path))]
}

// notOpenedText is the status line for an attachment that wasn't opened.
func notOpenedText(path string) string {
	what := "Files of this type"
	if ext := strings.ToLower(filepath.Ext(path)); len(ext) > 1 {
		what = oneLine(ext) + " files"
	}
	return "Not opened: " + what + " can run code on this computer. Press s to save it instead."
}

// launch starts the system's default app for a file or link. A variable
// so that tests can stand in for it.
var launch = openExternal

// openExternal opens a file or http(s) link with the system's default app.
// The target is passed as a single argument, never through a shell. Files
// received from others go through openable first.
func openExternal(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // don't leave a zombie behind
	return nil
}

// downloadsDir is where "save" puts media: ~/Downloads/WhatsApp.
func downloadsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads", "WhatsApp")
}
