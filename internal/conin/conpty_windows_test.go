//go:build windows

package conin

// This file is the test bench for the end-to-end tests in e2e_windows_test.go.
// It runs the echo helper (internal/echo) inside a pseudo console (ConPTY) and
// plays the part of the terminal: whatever the test writes arrives at the
// helper's console exactly as it would from Windows Terminal, which talks to
// console programs through this same mechanism.
//
// Two console hosts are exercised when available:
//
//   - "inbox": conhost.exe that ships with Windows, via CreatePseudoConsole.
//   - "wt": OpenConsole.exe from the installed Windows Terminal package, the
//     host a program actually gets when it runs inside Windows Terminal. It is
//     newer than the inbox one and behaves differently in places, so both
//     matter.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// bench holds what the tests build once and share: a scratch directory, the
// compiled echo helper, and a private copy of Windows Terminal's console host.
var bench struct {
	dirOnce sync.Once
	dir     string
	dirErr  error

	helperOnce sync.Once
	helperExe  string
	helperErr  error

	wtOnce sync.Once
	wtExe  string // "" if Windows Terminal is not installed
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bench.dir != "" {
		_ = os.RemoveAll(bench.dir)
	}
	os.Exit(code)
}

// benchDir returns the shared scratch directory, creating it on first use.
func benchDir() (string, error) {
	bench.dirOnce.Do(func() {
		bench.dir, bench.dirErr = os.MkdirTemp("", "conin-test-")
	})
	return bench.dir, bench.dirErr
}

// helperExe builds internal/echo and returns the path of the executable.
func helperExe(t *testing.T) string {
	t.Helper()
	bench.helperOnce.Do(func() {
		dir, err := benchDir()
		if err != nil {
			bench.helperErr = err
			return
		}
		bench.helperExe = filepath.Join(dir, "conin-echo.exe")
		// Tests run with the package directory as working directory.
		out, err := exec.Command("go", "build", "-o", bench.helperExe, "./internal/echo").CombinedOutput()
		if err != nil {
			bench.helperErr = fmt.Errorf("building the echo helper: %v\n%s", err, out)
		}
	})
	if bench.helperErr != nil {
		t.Fatal(bench.helperErr)
	}
	return bench.helperExe
}

// host names a console host program. path is empty for the one built into
// Windows.
type host struct {
	name string
	path string
}

// hosts lists the console hosts to test against.
func hosts() []host {
	list := []host{{name: "inbox"}}
	bench.wtOnce.Do(func() { bench.wtExe = copyOpenConsole() })
	if bench.wtExe != "" {
		list = append(list, host{name: "wt", path: bench.wtExe})
	}
	return list
}

// copyOpenConsole copies Windows Terminal's console host into the scratch
// directory and returns the copy's path, or "" if it cannot be found. A copy
// is needed because programs inside a Store app's folder may be read but not
// started from outside the app. Set CONIN_OPENCONSOLE to test a specific
// OpenConsole.exe instead.
func copyOpenConsole() string {
	source := os.Getenv("CONIN_OPENCONSOLE")
	if source == "" {
		source = findOpenConsole()
	}
	if source == "" {
		return ""
	}
	dir, err := benchDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return ""
	}
	dest := filepath.Join(dir, "OpenConsole.exe")
	if err := os.WriteFile(dest, data, 0o755); err != nil {
		return ""
	}
	return dest
}

// findOpenConsole looks up where Windows Terminal is installed. Store apps
// live in C:\Program Files\WindowsApps, a folder that may not be listed, so
// the location comes from the registry's record of the user's packages.
func findOpenConsole() string {
	const packages = `Software\Classes\Local Settings\Software\Microsoft\Windows\CurrentVersion\AppModel\Repository\Packages`
	key, err := registry.OpenKey(registry.CURRENT_USER, packages, registry.ENUMERATE_SUB_KEYS)
	if err != nil {
		return ""
	}
	defer key.Close()
	names, _ := key.ReadSubKeyNames(-1)
	sort.Strings(names)
	found := ""
	for _, name := range names {
		if !strings.HasPrefix(name, "Microsoft.WindowsTerminal") {
			continue
		}
		pkg, err := registry.OpenKey(key, name, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		root, _, err := pkg.GetStringValue("PackageRootFolder")
		pkg.Close()
		if err != nil {
			continue
		}
		exe := filepath.Join(root, "OpenConsole.exe")
		if _, err := os.Stat(exe); err == nil {
			found = exe
		}
	}
	return found
}

// forEachHost runs the test body once per console host, as a subtest.
func forEachHost(t *testing.T, body func(t *testing.T, h host)) {
	for _, h := range hosts() {
		t.Run(h.name, func(t *testing.T) { body(t, h) })
	}
}

// pty is a pseudo console with the echo helper running inside it.
type pty struct {
	t *testing.T

	input   windows.Handle // we write here: what the "terminal" sends
	output  windows.Handle // we read here: what the program draws
	process windows.Handle // the helper

	// Exactly one of these is set, depending on the host.
	inbox  windows.Handle // an HPCON from CreatePseudoConsole
	custom *customConsole // OpenConsole.exe started by hand

	logPath   string // the helper's -out file: one line per message
	tracePath string // the reader's WA_CONIN_TRACE file

	mu     sync.Mutex
	screen bytes.Buffer // everything read from output so far
	closed bool
}

// startPTY creates a pseudo console of 100x30 cells on the given host and
// runs the echo helper in it with the given extra arguments. It returns once
// the helper reports that it is reading input.
func startPTY(t *testing.T, h host, args ...string) *pty {
	t.Helper()
	exe := helperExe(t)
	dir := t.TempDir()
	p := &pty{
		t:         t,
		logPath:   filepath.Join(dir, "messages.log"),
		tracePath: filepath.Join(dir, "trace.log"),
	}

	// Two pipes: one carries input to the console, one carries its output.
	var inRead, outWrite windows.Handle
	must(t, windows.CreatePipe(&inRead, &p.input, nil, 0))
	must(t, windows.CreatePipe(&p.output, &outWrite, nil, 0))

	size := windows.Coord{X: 100, Y: 30}
	var consoleAttr unsafe.Pointer // the value for PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE
	if h.path == "" {
		must(t, windows.CreatePseudoConsole(size, inRead, outWrite, 0, &p.inbox))
		// The attribute's value is the HPCON itself. It is a pointer in
		// disguise, so reinterpret the variable rather than convert the
		// number (which "go vet" rightly distrusts).
		consoleAttr = *(*unsafe.Pointer)(unsafe.Pointer(&p.inbox))
	} else {
		p.custom = startCustomConsole(t, h.path, size, inRead, outWrite)
		consoleAttr = unsafe.Pointer(p.custom)
	}
	// The console host has its own copies of these ends now.
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)

	go p.readOutput()

	// Start the helper attached to the pseudo console.
	attrs, err := windows.NewProcThreadAttributeList(1)
	must(t, err)
	defer attrs.Delete()
	must(t, attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, consoleAttr, unsafe.Sizeof(p.inbox)))

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	// With this flag and no handles given, the helper gets fresh standard
	// handles for the pseudo console instead of inheriting the test's own
	// (redirected) ones.
	si.Flags = windows.STARTF_USESTDHANDLES
	si.ProcThreadAttributeList = attrs.List()

	cmdLine := windows.ComposeCommandLine(append([]string{exe, "-out", p.logPath}, args...))
	cmdLinePtr, err := windows.UTF16PtrFromString(cmdLine)
	must(t, err)
	env := environmentBlock(append(os.Environ(), "WA_CONIN_TRACE="+p.tracePath))

	var pi windows.ProcessInformation
	must(t, windows.CreateProcess(nil, cmdLinePtr, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&env[0], nil, &si.StartupInfo, &pi))
	_ = windows.CloseHandle(pi.Thread)
	p.process = pi.Process

	t.Cleanup(p.close)

	// Wait for the helper to say it is ready (READY in plain mode, STARTED
	// in -tea mode).
	p.waitFor(10*time.Second, "the helper to start", func(lines []string) bool {
		for _, line := range lines {
			if strings.HasPrefix(line, "READY") || strings.HasPrefix(line, "STARTED") {
				return true
			}
			if strings.HasPrefix(line, "ERROR") || strings.HasPrefix(line, "UNSUPPORTED") {
				t.Fatalf("helper failed to start: %s", line)
			}
		}
		return false
	})
	// Give the console host a moment to finish its own start-up chatter
	// with the "terminal", so it cannot interleave with a test's input.
	time.Sleep(150 * time.Millisecond)
	return p
}

// readOutput collects everything the console host writes to the terminal
// side, and answers the questions a host asks its terminal. Newer hosts stop
// processing input for up to three seconds while they wait for an answer, so
// a terminal that stays silent would distort every timing in these tests.
func (p *pty) readOutput() {
	// Each question, and the answer Windows Terminal would give.
	questions := []struct{ ask, answer string }{
		// Device Attributes: "what kind of terminal are you?" Asked at start-up.
		{"\x1b[c", "\x1b[?61;4;6;7;14;21;22;23;24;28;32;42c"},
		// Cursor Position Report: "where is your cursor?" Asked after resizes
		// and some screen changes.
		{"\x1b[6n", "\x1b[1;1R"},
	}
	buf := make([]byte, 16*1024)
	scanned := 0 // how much of p.screen has been searched for questions
	for {
		var n uint32
		if err := windows.ReadFile(p.output, buf, &n, nil); err != nil || n == 0 {
			return
		}
		p.mu.Lock()
		p.screen.Write(buf[:n])
		all := p.screen.Bytes()
		var answers []string
		for scanned < len(all) {
			matched := false
			for _, q := range questions {
				if bytes.HasPrefix(all[scanned:], []byte(q.ask)) {
					answers = append(answers, q.answer)
					scanned += len(q.ask)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
			// A question may be cut in two by the read: keep its beginning
			// for next time.
			if all[scanned] == 0x1b && len(all)-scanned < 4 {
				break
			}
			scanned++
		}
		p.mu.Unlock()
		for _, answer := range answers {
			p.write(answer)
		}
	}
}

// write sends bytes to the console as terminal input, in a single write.
func (p *pty) write(s string) {
	var n uint32
	if err := windows.WriteFile(p.input, []byte(s), &n, nil); err != nil {
		p.t.Errorf("writing to the pty: %v", err)
	}
}

// resize changes the pseudo console's size, as dragging the terminal
// window's border would.
func (p *pty) resize(width, height int) {
	size := windows.Coord{X: int16(width), Y: int16(height)}
	if p.custom != nil {
		p.custom.resize(p.t, size)
		return
	}
	must(p.t, windows.ResizePseudoConsole(p.inbox, size))
}

// screenText returns everything the console host has written so far.
func (p *pty) screenText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.screen.String()
}

// lines returns the helper's log, one entry per line.
func (p *pty) lines() []string {
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// trace returns the reader's own trace of what it read, for failure output.
func (p *pty) trace() string {
	data, _ := os.ReadFile(p.tracePath)
	return string(data)
}

// waitFor polls the helper's log until done reports true, failing the test
// after timeout.
func (p *pty) waitFor(timeout time.Duration, what string, done func(lines []string) bool) []string {
	p.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		lines := p.lines()
		if done(lines) {
			return lines
		}
		if time.Now().After(deadline) {
			p.t.Fatalf("timed out waiting for %s.\nhelper log:\n%s\nreader trace:\n%s",
				what, strings.Join(lines, "\n"), p.trace())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// close ends the helper and the console host.
func (p *pty) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.mu.Unlock()

	_ = windows.TerminateProcess(p.process, 0)
	_, _ = windows.WaitForSingleObject(p.process, 2000)
	_ = windows.CloseHandle(p.process)
	if p.custom != nil {
		p.custom.close()
	} else {
		windows.ClosePseudoConsole(p.inbox)
	}
	_ = windows.CloseHandle(p.input)
	_ = windows.CloseHandle(p.output) // also ends readOutput
}

// waitExit waits for the helper to exit on its own and returns its exit code.
func (p *pty) waitExit(timeout time.Duration) uint32 {
	p.t.Helper()
	event, err := windows.WaitForSingleObject(p.process, uint32(timeout/time.Millisecond))
	if err != nil || event != windows.WAIT_OBJECT_0 {
		p.t.Fatalf("the helper did not exit within %v.\nhelper log:\n%s", timeout, strings.Join(p.lines(), "\n"))
	}
	var code uint32
	must(p.t, windows.GetExitCodeProcess(p.process, &code))
	return code
}

// customConsole is a pseudo console run by a console host of our choosing
// instead of the one built into Windows. This reproduces what the open-source
// conpty library inside Windows Terminal does (winconpty.cpp).
//
// The three handles at the start are, in this order, the memory layout
// Windows expects behind an HPCON; a pointer to this struct can therefore be
// passed as PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE.
type customConsole struct {
	signal    windows.Handle // pipe for control messages (resize) to the host
	reference windows.Handle // names the console a new process attaches to
	process   windows.Handle // the console host
}

func startCustomConsole(t *testing.T, hostPath string, size windows.Coord, inRead, outWrite windows.Handle) *customConsole {
	t.Helper()

	// A console host serves one "console driver" connection. Open the
	// server end (inheritable, for the host) …
	server := openConsoleDriver(t, `\Device\ConDrv\Server`, 0,
		windows.GENERIC_ALL, windows.OBJ_INHERIT, 0)
	// … and a reference to it, which is how client processes find it.
	reference := openConsoleDriver(t, `\Reference`, server,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE, 0, windows.FILE_SYNCHRONOUS_IO_NONALERT)

	var signalRead, signalWrite windows.Handle
	must(t, windows.CreatePipe(&signalRead, &signalWrite, nil, 0))

	// The host must inherit exactly these four handles.
	inherit := []windows.Handle{server, inRead, outWrite, signalRead}
	for _, h := range inherit {
		must(t, windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT))
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	must(t, err)
	defer attrs.Delete()
	must(t, attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&inherit[0]), uintptr(len(inherit))*unsafe.Sizeof(inherit[0])))

	var si windows.StartupInfoEx
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput = inRead
	si.StdOutput = outWrite
	si.StdErr = outWrite
	si.ProcThreadAttributeList = attrs.List()

	cmdLine := fmt.Sprintf(`"%s" --headless --width %d --height %d --signal 0x%x --server 0x%x`,
		hostPath, size.X, size.Y, uintptr(signalRead), uintptr(server))
	cmdLinePtr, err := windows.UTF16PtrFromString(cmdLine)
	must(t, err)

	var pi windows.ProcessInformation
	must(t, windows.CreateProcess(nil, cmdLinePtr, nil, nil, true,
		windows.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &si.StartupInfo, &pi))
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(server)
	_ = windows.CloseHandle(signalRead)

	return &customConsole{signal: signalWrite, reference: reference, process: pi.Process}
}

// openConsoleDriver opens an object of the console driver by its native
// (NT) name, optionally relative to another handle.
func openConsoleDriver(t *testing.T, name string, parent windows.Handle, access, attributes, options uint32) windows.Handle {
	t.Helper()
	ntName, err := windows.NewNTUnicodeString(name)
	must(t, err)
	oa := windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    ntName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | attributes,
	}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	must(t, windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, options, 0, 0))
	return h
}

// resize tells the host about a new window size through the signal pipe.
func (c *customConsole) resize(t *testing.T, size windows.Coord) {
	const signalResizeWindow = 8 // PTY_SIGNAL_RESIZE_WINDOW
	msg := []byte{signalResizeWindow, 0, byte(size.X), byte(size.X >> 8), byte(size.Y), byte(size.Y >> 8)}
	var n uint32
	must(t, windows.WriteFile(c.signal, msg, &n, nil))
}

func (c *customConsole) close() {
	_ = windows.CloseHandle(c.reference)
	_ = windows.CloseHandle(c.signal)
	_ = windows.TerminateProcess(c.process, 0)
	_ = windows.CloseHandle(c.process)
}

// environmentBlock encodes KEY=value strings the way CreateProcess wants
// them: UTF-16, each terminated by a zero, with one more zero at the end.
func environmentBlock(env []string) []uint16 {
	var block []uint16
	for _, kv := range env {
		block = append(block, utf16.Encode([]rune(kv))...)
		block = append(block, 0)
	}
	return append(block, 0)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
