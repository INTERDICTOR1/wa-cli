//go:build windows

package conin

// End-to-end tests: the real reader, in a real console program, inside a real
// pseudo console, fed the bytes a terminal would send. See
// conpty_windows_test.go for the test bench.
//
// They are skipped with "go test -short" (they need the go tool to build the
// helper and take about half a minute).

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- what the helper prints --------------------------------------------------

// These build the text the echo helper prints for a message, so expectations
// read naturally: typed("enter"), runes("h"), pasteOf("a\nb").

func typed(keyName string) string {
	return fmt.Sprintf(`key type=%q runes="" paste=false alt=false`, keyName)
}

func runes(s string) string {
	return fmt.Sprintf(`key type="runes" runes=%q paste=false alt=false`, s)
}

func pasteOf(s string) string {
	return fmt.Sprintf(`key type="runes" runes=%q paste=true alt=false`, s)
}

// win32Key encodes one key event in "win32-input-mode", the format Windows
// Terminal uses to send key presses to a console host:
// ESC [ Vk ; Sc ; Uc ; Kd ; Cs ; Rc _  (virtual key, scan code, character,
// down?, control-key state, repeat count).
func win32Key(vk, scan int, char rune, isDown bool, mods uint32) string {
	d := 0
	if isDown {
		d = 1
	}
	return fmt.Sprintf("\x1b[%d;%d;%d;%d;%d;1_", vk, scan, char, d, mods)
}

// press is a key going down and coming up again.
func press(vk, scan int, char rune, mods uint32) string {
	return win32Key(vk, scan, char, true, mods) + win32Key(vk, scan, char, false, mods)
}

var msgLine = regexp.MustCompile(`^MSG at=(\d+) (.*)$`)

// messages returns the helper's messages of the given kind ("key", "mouse",
// "size", "focus", "blur"; "" for all), and when each was received (Unix
// microseconds).
func (p *pty) messages(kind string) (texts []string, times []int64) {
	for _, line := range p.lines() {
		m := msgLine.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(m[2], kind) {
			continue
		}
		at, _ := strconv.ParseInt(m[1], 10, 64)
		texts = append(texts, m[2])
		times = append(times, at)
	}
	return texts, times
}

// expect waits until the helper has reported the wanted messages of the given
// kind, then makes sure they are exactly those and that nothing follows.
func (p *pty) expect(kind string, want ...string) {
	p.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if got, _ := p.messages(kind); len(got) >= len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Anything that was going to leak out after a paste (the bug this
	// package exists to prevent is a stray Enter) would do so well within
	// this pause.
	time.Sleep(250 * time.Millisecond)

	got, _ := p.messages(kind)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		p.t.Errorf("%s messages differ\n got:\n\t%s\nwant:\n\t%s\nreader trace:\n%s",
			kind, strings.Join(got, "\n\t"), strings.Join(want, "\n\t"), clip(p.trace(), 6000))
	}
}

// clip shortens long text for failure messages.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n… (%d more bytes)", len(s)-max)
}

func skipIfShort(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end ConPTY test skipped in -short mode")
	}
}

// --- the cases ---------------------------------------------------------------

// Case a: three lines pasted in one write arrive as ONE paste message, and no
// Enter key.
func TestE2EPasteIsOneMessage(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.write("hello\rworld\rthird line")
		p.expect("key", pasteOf("hello\nworld\nthird line"))
	})
}

// Case b: a lone Enter is the Enter key, delivered without delay.
func TestE2ELoneEnterIsPrompt(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		const rounds = 20
		var sent []int64
		var want []string
		for i := 0; i < rounds; i++ {
			sent = append(sent, time.Now().UnixMicro())
			p.write("\r")
			want = append(want, typed("enter"))
			time.Sleep(60 * time.Millisecond)
		}
		p.expect("key", want...)

		_, received := p.messages("key")
		if len(received) != rounds {
			return // expect has already reported it
		}
		var latency []time.Duration
		for i := range received {
			latency = append(latency, time.Duration(received[i]-sent[i])*time.Microsecond)
		}
		sort.Slice(latency, func(i, j int) bool { return latency[i] < latency[j] })
		worst := latency[len(latency)-1]
		t.Logf("Enter latency over %d presses (write to pty -> message received): median %v, worst %v",
			rounds, latency[len(latency)/2], worst)
		if worst > 50*time.Millisecond {
			t.Errorf("slowest Enter took %v; want well under 50ms", worst)
		}
	})
}

// Case c: characters arriving one at a time, 60 ms apart, are typing and not
// a paste.
func TestE2ETypingIsNotAPaste(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		var want []string
		for _, ch := range "hello, world" {
			p.write(string(ch))
			want = append(want, runes(string(ch)))
			time.Sleep(60 * time.Millisecond)
		}
		p.write("\r")
		want = append(want, typed("enter"))
		p.expect("key", want...)
	})
}

// Case d: a bracketed paste. Both console hosts on this machine strip the
// ESC[200~ / ESC[201~ markers themselves and deliver only the text (so the
// reader recognises it as a burst); the test accepts either route to the one
// paste message. It makes no difference whether the program has asked the
// terminal for bracketed paste (as Bubble Tea does) or not.
func TestE2EBracketedPaste(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		for _, args := range [][]string{nil, {"-bp"}} {
			p := startPTY(t, h, args...)
			p.write("\x1b[200~a\rb\x1b[201~")
			p.expect("key", pasteOf("a\nb"))

			markersSeen := strings.Contains(p.trace(), `ch='\x1b'`)
			requested := strings.Contains(p.screenText(), "\x1b[?2004h")
			t.Logf("args %v: paste markers reached the reader: %v; ESC[?2004h forwarded to the terminal: %v",
				args, markersSeen, requested)
			p.close()
		}
	})
}

// Case e: an emoji outside the Basic Multilingual Plane is one key with one
// rune, not two broken halves.
func TestE2EEmojiIsOneRune(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.write("\U0001F600")
		p.expect("key", runes("\U0001F600"))
	})
}

// Case f: keys sent the way Windows Terminal sends them (win32-input-mode),
// which is what carries the modifier information.
func TestE2EModifiedKeys(t *testing.T) {
	skipIfShort(t)
	const (
		shift = shiftPressed
		ctrl  = leftCtrlPressed
		alt   = leftAltPressed
	)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		steps := []struct {
			send string
			want string
		}{
			{press(vkReturn, 0x1C, '\r', 0), typed("enter")},
			{press(vkReturn, 0x1C, '\r', shift), typed("ctrl+j")}, // Shift+Enter
			{press(vkReturn, 0x1C, '\n', ctrl), typed("ctrl+j")},  // Ctrl+Enter
			{press(vkReturn, 0x1C, '\r', alt), `key type="enter" runes="" paste=false alt=true`},
			{press('C', 0x2E, 0x03, ctrl), typed("ctrl+c")},
			{press(vkEscape, 0x01, 0x1b, 0), typed("esc")},
			{press('X', 0x2D, 'x', alt), `key type="runes" runes="x" paste=false alt=true`},
			{press('Q', 0x10, '@', leftCtrlPressed|rightAltPressed), runes("@")}, // AltGr+Q on a German keyboard
			{press(vkTab, 0x0F, '\t', shift), typed("shift+tab")},
			{press(vkLeft, 0x4B, 0, ctrl), typed("ctrl+left")},
			{press(vkBack, 0x0E, '\b', 0), typed("backspace")},
		}
		var want []string
		for _, s := range steps {
			p.write(s.send)
			want = append(want, s.want)
			time.Sleep(60 * time.Millisecond)
		}
		p.expect("key", want...)
	})
}

// A lone Esc is held back only briefly (to see whether a paste marker
// follows).
func TestE2EEscapeLatency(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		sent := time.Now().UnixMicro()
		p.write(win32Key(vkEscape, 0x01, 0x1b, true, 0)) // key down only: the finger is still on it
		p.expect("key", typed("esc"))
		if _, received := p.messages("key"); len(received) == 1 {
			latency := time.Duration(received[0]-sent) * time.Microsecond
			t.Logf("Esc latency: %v", latency)
			if latency > 100*time.Millisecond {
				t.Errorf("Esc took %v", latency)
			}
		}
	})
}

// Case g: a large paste (200 lines, about 19 KB) arrives complete and in
// order, as paste messages only — never a bare Enter.
func TestE2ELargePaste(t *testing.T) {
	skipIfShort(t)
	var text strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&text, "line %03d: the quick brown fox jumps over the lazy dog, again and again and again and again.\r", i)
	}
	want := strings.ReplaceAll(text.String(), "\r", "\n")

	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.write(text.String())

		// Wait until all of the text has been reported, then a little
		// longer for anything that should not be there.
		collected := func() (joined string, pastes int, others []string) {
			texts, _ := p.messages("key")
			for _, m := range texts {
				var got string
				if _, err := fmt.Sscanf(m, `key type="runes" runes=%q paste=true alt=false`, &got); err != nil {
					others = append(others, m)
					continue
				}
				joined += got
				pastes++
			}
			return joined, pastes, others
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			if joined, _, _ := collected(); len(joined) >= len(want) || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(250 * time.Millisecond)

		joined, pastes, others := collected()
		if len(others) > 0 {
			t.Errorf("non-paste key messages leaked out of the paste: %q", others)
		}
		if joined != want {
			t.Errorf("pasted text differs: got %d bytes, want %d bytes", len(joined), len(want))
		}
		reads := strings.Count(p.trace(), "ms  read ")
		t.Logf("%d bytes arrived in %d paste message(s), over %d reads of the console", len(joined), pastes, reads)
	})
}

// Pasted text with characters that are not on the keyboard layout, capitals
// (which the console wraps in Shift presses) and tabs.
func TestE2EUnicodePaste(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.write("don’t → € é 中 \U0001F600\tTab AAA aaa\r\r")
		p.expect("key", pasteOf("don’t → € é 中 \U0001F600\tTab AAA aaa\n\n"))
	})
}

// A terminal that sends LF or CR LF for line breaks instead of CR.
func TestE2ELineFeeds(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.write("a\nb")
		time.Sleep(200 * time.Millisecond)
		p.write("c\r\nd")
		p.expect("key", pasteOf("a\nb"), pasteOf("c\nd"))
	})
}

func TestE2EMouse(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h, "-mouse")
		// SGR mouse reports, as a terminal sends them: ESC [ < button ; x ; y
		// then M (press) or m (release). Coordinates are 1-based.
		for _, report := range []string{
			"\x1b[<64;10;5M", // wheel up
			"\x1b[<65;10;5M", // wheel down
			"\x1b[<0;10;5M",  // left button down
			"\x1b[<0;10;5m",  // left button up
			"\x1b[<35;11;6M", // movement with no button: must be dropped
		} {
			p.write(report)
			time.Sleep(60 * time.Millisecond)
		}
		p.expect("mouse",
			`mouse event="wheel up" x=9 y=4`,
			`mouse event="wheel down" x=9 y=4`,
			`mouse event="left press" x=9 y=4`,
			`mouse event="left release" x=9 y=4`,
		)
		t.Logf("console host asked the terminal for mouse reports (ESC[?1003;1006h): %v",
			strings.Contains(p.screenText(), "\x1b[?1003;1006h"))
	})
}

func TestE2EFocus(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.write("\x1b[O") // the terminal window lost focus
		time.Sleep(60 * time.Millisecond)
		p.write("\x1b[I") // and got it back
		p.expect("", "blur", "focus")
	})
}

func TestE2EResize(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		p := startPTY(t, h)
		p.resize(120, 40)
		p.expect("size", "size w=120 h=40")
	})
}

// Stop puts the console's input mode back exactly as it was.
func TestE2EStopRestoresConsoleMode(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		for _, c := range []struct {
			args     []string
			wantMode string
		}{
			{nil, "0x88"},                // ENABLE_WINDOW_INPUT | ENABLE_EXTENDED_FLAGS
			{[]string{"-mouse"}, "0x98"}, // … | ENABLE_MOUSE_INPUT
		} {
			p := startPTY(t, h, c.args...)
			p.write(press(0x7B, 0x58, 0, 0)) // F12: the helper calls Stop (twice) and exits
			if code := p.waitExit(5 * time.Second); code != 0 {
				t.Errorf("helper exit code %d", code)
			}
			log := strings.Join(p.lines(), "\n")
			var before, during, after string
			for _, line := range p.lines() {
				fmt.Sscanf(line, "READY in-before=%s in-now=%s", &before, &during)
				fmt.Sscanf(line, "STOPPED in-after=%s", &after)
			}
			if during != c.wantMode {
				t.Errorf("input mode while reading = %s, want %s\n%s", during, c.wantMode, log)
			}
			if before == "" || after != before {
				t.Errorf("input mode before = %q, after Stop = %q\n%s", before, after, log)
			}
			p.close()
		}
	})
}

// The smoke test for the wiring the TUI uses: a Bubble Tea program started
// with tea.WithInput(nil) and fed by conin.Start(p.Send, …).
func TestE2EBubbleTea(t *testing.T) {
	skipIfShort(t)
	forEachHost(t, func(t *testing.T, h host) {
		// -mouse: conin.Options{Mouse: true}. The Bubble Tea program is NOT
		// given tea.WithMouseCellMotion; conin alone must deliver the mouse.
		p := startPTY(t, h, "-tea", "-mouse")

		// teaSaw waits for a line "TEA at=… <text>" in the helper's log.
		teaSaw := func(text string) {
			t.Helper()
			p.waitFor(10*time.Second, fmt.Sprintf("Update to receive %q", text), func(lines []string) bool {
				for _, line := range lines {
					if strings.HasPrefix(line, "TEA ") && strings.HasSuffix(line, " "+text) {
						return true
					}
				}
				return false
			})
		}
		// drew waits for text to appear in what the program drew.
		drew := func(text string) {
			t.Helper()
			deadline := time.Now().Add(10 * time.Second)
			for !strings.Contains(p.screenText(), text) {
				if time.Now().After(deadline) {
					t.Fatalf("the program never drew %q; output so far:\n%q", text, clip(p.screenText(), 4000))
				}
				time.Sleep(10 * time.Millisecond)
			}
		}

		// Bubble Tea itself still reports the initial window size …
		teaSaw("size w=100 h=30")
		// … and still draws (it has switched the console to VT output).
		drew("conin smoke test")
		drew("size=100x30")

		// A key sent through conin -> Program.Send reaches Update.
		p.write("x")
		teaSaw(runes("x"))
		drew(`last="x"`)

		// So does a paste, as one message.
		p.write("two\rlines")
		teaSaw(pasteOf("two\nlines"))

		// Resizes come from conin (Bubble Tea has no other source for them
		// on Windows).
		p.resize(120, 40)
		teaSaw("size w=120 h=40")
		drew("size=120x40")

		// The mouse wheel, with only conin asking for the mouse.
		p.write("\x1b[<64;10;5M")
		teaSaw(`mouse event="wheel up" x=9 y=4`)

		// Quit, and check that both console modes are back as they were.
		p.write("q")
		if code := p.waitExit(5 * time.Second); code != 0 {
			t.Errorf("helper exit code %d", code)
		}
		log := strings.Join(p.lines(), "\n")
		var inBefore, outBefore, inRunning, outRunning, inAfter, outAfter, exitErr string
		for _, line := range p.lines() {
			fmt.Sscanf(line, "BEFORE in=%s out=%s", &inBefore, &outBefore)
			fmt.Sscanf(line, "RUNNING in=%s out=%s", &inRunning, &outRunning)
			fmt.Sscanf(line, "EXIT in=%s out=%s err=%s", &inAfter, &outAfter, &exitErr)
		}
		t.Logf("console modes: before in=%s out=%s; running in=%s out=%s; after exit in=%s out=%s; Run error: %s",
			inBefore, outBefore, inRunning, outRunning, inAfter, outAfter, exitErr)
		if inRunning != "0x98" {
			t.Errorf("input mode while running = %s, want 0x98 (conin's, with mouse)\n%s", inRunning, log)
		}
		if inBefore == "" || inAfter != inBefore || outAfter != outBefore {
			t.Errorf("console modes not restored\n%s", log)
		}
		if exitErr != "<nil>" {
			t.Errorf("Program.Run returned %s", exitErr)
		}
	})
}
