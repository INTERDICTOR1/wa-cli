//go:build windows

package tui

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This test runs the real `wa` binary in demo mode inside a Windows pseudo
// console - the same mechanism Windows Terminal uses to host console
// programs - and types and pastes into it as a terminal would. It is the
// end-to-end check for the bug this interface was rebuilt around: pasted
// text with line breaks used to go out as one message per line.

// console is a pseudo console with a program running in it.
type console struct {
	t       *testing.T
	input   windows.Handle // what the "terminal" sends
	output  windows.Handle // what the program draws
	process windows.Handle
	pcon    windows.Handle

	mu     sync.Mutex
	screen bytes.Buffer
}

func startConsole(t *testing.T, exe string, env []string, cols, rows int) *console {
	t.Helper()
	c := &console{t: t}
	var inRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &c.input, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePipe(&c.output, &outWrite, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inRead, outWrite, 0, &c.pcon); err != nil {
		t.Skipf("pseudo consoles aren't available here: %v", err)
	}
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	go c.read()

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		t.Fatal(err)
	}
	defer attrs.Delete()
	// The attribute's value is the console handle itself (a pointer in
	// disguise), so reinterpret the variable rather than convert it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&c.pcon)), unsafe.Sizeof(c.pcon)); err != nil {
		t.Fatal(err)
	}
	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES // fresh console handles, not the test's
	si.ProcThreadAttributeList = attrs.List()

	cmdLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{exe}))
	if err != nil {
		t.Fatal(err)
	}
	var block []uint16
	for _, kv := range env {
		block = append(block, windows.StringToUTF16(kv)...)
	}
	block = append(block, 0)

	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cmdLine, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&block[0], nil, &si.StartupInfo, &pi); err != nil {
		t.Fatal(err)
	}
	_ = windows.CloseHandle(pi.Thread)
	c.process = pi.Process
	t.Cleanup(c.close)
	return c
}

func (c *console) read() {
	buf := make([]byte, 16<<10)
	answered := false
	for {
		var n uint32
		if err := windows.ReadFile(c.output, buf, &n, nil); err != nil || n == 0 {
			return
		}
		c.mu.Lock()
		c.screen.Write(buf[:n])
		// Newer console hosts ask the terminal what it is ("ESC [ c") and
		// wait for the answer before starting the program.
		ask := !answered && bytes.Contains(c.screen.Bytes(), []byte("\x1b[c"))
		answered = answered || ask
		c.mu.Unlock()
		if ask {
			c.write("\x1b[?61;4;6;7;14;21;22;23;24;28;32;42c")
		}
	}
}

// write sends bytes as terminal input, in one write (as a paste arrives).
func (c *console) write(s string) {
	var n uint32
	if err := windows.WriteFile(c.input, []byte(s), &n, nil); err != nil {
		c.t.Errorf("writing to the console: %v", err)
	}
}

func (c *console) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.screen.String()
}

// waitFor polls until done reports true, failing after timeout.
func (c *console) waitFor(timeout time.Duration, what string, done func() bool) {
	c.t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			out := c.text()
			if len(out) > 3000 {
				out = out[len(out)-3000:]
			}
			c.t.Fatalf("timed out waiting for %s. Last output:\n%q", what, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *console) waitForText(timeout time.Duration, s string) {
	c.t.Helper()
	c.waitFor(timeout, "the screen to show "+s, func() bool { return strings.Contains(c.text(), s) })
}

func (c *console) exitCode(timeout time.Duration) (uint32, bool) {
	if ev, err := windows.WaitForSingleObject(c.process, uint32(timeout/time.Millisecond)); err != nil || ev != windows.WAIT_OBJECT_0 {
		return 0, false
	}
	var code uint32
	_ = windows.GetExitCodeProcess(c.process, &code)
	return code, true
}

func (c *console) close() {
	_ = windows.TerminateProcess(c.process, 1)
	_, _ = windows.WaitForSingleObject(c.process, 2000)
	_ = windows.CloseHandle(c.process)
	windows.ClosePseudoConsole(c.pcon)
	_ = windows.CloseHandle(c.input)
	_ = windows.CloseHandle(c.output)
}

func TestEndToEndInAPseudoConsole(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the real binary")
	}
	dir := t.TempDir()
	exe := filepath.Join(dir, "wa-e2e.exe")
	pkg := "github.com/codebyoketch/wa-cli"
	if override := os.Getenv("WA_E2E_PKG"); override != "" {
		pkg = override // a smaller main package that also runs the TUI
	}
	build := exec.Command("go", "build", "-o", exe, pkg)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building wa: %v\n%s", err, out)
	}

	sentLog := filepath.Join(dir, "sent.log")
	sent := func() []string {
		data, err := os.ReadFile(sentLog)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	env := append(os.Environ(),
		"WA_DEMO=1",
		"WA_DEMO_LOG="+sentLog,
		"APPDATA="+filepath.Join(dir, "appdata"), // keep the real config and data out of it
		"WT_SESSION=e2e", // as inside Windows Terminal: photos are drawn with Sixel
	)
	// The one-time import of the old JSON stores runs as a helper process
	// (the hidden command Run starts by itself when it finds them).
	dataDir := filepath.Join(dir, "appdata", "wa", "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]string{
		"chats.json":    `{"1@s.whatsapp.net":{"jid":"1@s.whatsapp.net","name":"Old Chat","lastMessageAt":5}}`,
		"messages.json": `{"1@s.whatsapp.net":[{"id":"A","chatJid":"1@s.whatsapp.net","senderJid":"1@s.whatsapp.net","timestamp":5,"text":"hi"}]}`,
	}
	for name, content := range legacy {
		if err := os.WriteFile(filepath.Join(dataDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !needsMigration(dataDir) {
		t.Fatal("old stores present, but no migration wanted")
	}
	migrate := exec.Command(exe, MigrateCommand)
	migrate.Env = env
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", MigrateCommand, err, out)
	}
	for _, name := range []string{"chats.json.migrated", "messages.json.migrated", "wa.db"} {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err != nil {
			t.Errorf("after migrating: %v", err)
		}
	}
	if needsMigration(dataDir) {
		t.Error("migration still wanted after it ran")
	}

	c := startConsole(t, exe, env, 110, 34)

	// The chat list appears; Enter opens the first chat for writing.
	c.waitForText(15*time.Second, "Aisha Khan")
	c.waitForText(5*time.Second, "Weekend Trip")
	c.write("\r")
	c.waitForText(5*time.Second, "enter send")

	// A multi-line paste, arriving in one piece as from a terminal: it
	// must fill the box, not send anything.
	c.write("first line\rsecond line\rthird line")
	c.waitForText(5*time.Second, "third line")
	time.Sleep(400 * time.Millisecond)
	if got := sent(); len(got) != 0 {
		t.Fatalf("pasting sent %d message(s) by itself: %q", len(got), got)
	}

	// One deliberate Enter sends it all as a single message.
	c.write("\r")
	c.waitFor(5*time.Second, "the message to be sent", func() bool { return len(sent()) > 0 })
	time.Sleep(300 * time.Millisecond)
	got := sent()
	if len(got) != 1 {
		t.Fatalf("sent %d messages, want exactly 1: %q", len(got), got)
	}
	if !strings.Contains(got[0], `text="first line\nsecond line\nthird line"`) {
		t.Fatalf("sent %s", got[0])
	}

	// Typed text and Enter still send right away.
	for _, ch := range "typed" {
		c.write(string(ch))
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	c.write("\r")
	c.waitFor(5*time.Second, "the typed message to be sent", func() bool { return len(sent()) == 2 })
	if got := sent(); !strings.Contains(got[1], `text="typed"`) {
		t.Fatalf("sent %s", got[1])
	}

	// The photo viewer: select the photo (above the messages just sent),
	// open it full screen with real pixels (Sixel, as in Windows Terminal),
	// and close it again.
	for i := 0; i < 4; i++ {
		c.write("\x1b[A") // up
		time.Sleep(80 * time.Millisecond)
	}
	c.waitForText(5*time.Second, "r reply")
	c.write("\r")
	c.waitForText(10*time.Second, "any key closes")
	// The caption says "loading…" until the picture has been decoded,
	// scaled and written out; then it names the drawing mode.
	c.waitFor(10*time.Second, "the photo to be drawn", func() bool {
		out := c.text()
		after := out[strings.LastIndex(out, "loading"):] // what was drawn after the "loading…" caption
		return strings.Contains(after, "1600×1000") && strings.Contains(after, "sixel") && strings.Contains(after, "any key closes")
	})
	if out := c.text(); strings.Contains(out, "Couldn't show this photo") || strings.Contains(out, "too large to draw") {
		t.Fatal("the viewer reported that it could not draw the photo")
	}
	c.write("x")
	c.waitFor(5*time.Second, "the viewer to close", func() bool {
		out := c.text()
		return strings.LastIndex(out, "r reply") > strings.LastIndex(out, "any key closes")
	})

	// Ctrl+C quits cleanly.
	c.write("\x03")
	code, exited := c.exitCode(5 * time.Second)
	if !exited || code != 0 {
		t.Fatalf("ctrl+c: exited=%v code=%d", exited, code)
	}
}
