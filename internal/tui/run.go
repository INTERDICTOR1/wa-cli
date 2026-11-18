package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/codebyoketch/wa-cli/internal/app"
	"github.com/codebyoketch/wa-cli/internal/chatstore"
	"github.com/codebyoketch/wa-cli/internal/conin"
	"github.com/codebyoketch/wa-cli/internal/msgstore"
	"github.com/codebyoketch/wa-cli/internal/store"
	"github.com/codebyoketch/wa-cli/internal/termimg"
	"github.com/codebyoketch/wa-cli/internal/whatsapp"
)

// memoryLimit is the Go runtime's soft memory target: the garbage
// collector works harder as the heap approaches it. It is set well below
// the 100 MB budget because SQLite's memory and the program's own code
// and data live outside the Go heap.
const memoryLimit = 64 << 20

// MigrateCommand is the hidden `wa` subcommand that runs Migrate.
const MigrateCommand = "migrate-store"

// legacyFiles are the old JSON stores; they are renamed once imported.
var legacyFiles = []string{"chats.json", "messages.json"}

// needsMigration reports whether the data directory still holds the old
// JSON stores, i.e. the one-time import into wa.db hasn't happened yet.
func needsMigration(dataDir string) bool {
	for _, name := range legacyFiles {
		if info, err := os.Stat(filepath.Join(dataDir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// Migrate does the one-time import of the old JSON stores into wa.db and
// the clean-up that goes with it (reactions and edits folded into their
// messages, duplicate chats merged). Purely local: it never connects.
//
// Run starts it in a separate, short-lived process. The import is the
// most memory-hungry thing wa-cli ever does (it parses the whole old
// history at once), and memory a process has used is not all given back;
// doing it elsewhere keeps the long-running TUI lean from its first run.
func Migrate(a *app.App) error {
	ctx := context.Background()
	container, err := store.Open(ctx, a.Config.DataDir, waLog.Noop)
	if err != nil {
		return err
	}
	defer container.Close()
	cs := chatstore.New(a.Config.DataDir)
	ms := msgstore.New(a.Config.DataDir)
	defer ms.Close()
	// A store imports its old JSON file the first time it is used.
	if _, err := cs.List(); err != nil {
		return err
	}
	if _, err := ms.Count(""); err != nil {
		return err
	}
	client, err := whatsapp.New(ctx, container, waLog.Noop, cs, ms)
	if err != nil {
		return err
	}
	client.Maintain(ctx)
	return nil
}

// migrateInChild runs Migrate in a child process and waits for it. Any
// failure is left for the TUI's own start-up to deal with (it does the
// same work in-process if it is still needed).
func migrateInChild() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, "Upgrading local chat history (one time)…")
	cmd := exec.Command(exe, MigrateCommand)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "wa: the upgrade will be retried:", err)
	}
}

// Run connects, launches the full-screen TUI, and blocks until the user
// quits (Ctrl+C). Requires an existing login (`wa login`).
//
// With WA_DEMO=1 it runs against a pretend in-memory WhatsApp instead:
// no login, no connection, nothing is really sent.
func Run(a *app.App) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := Options{
		Mouse:         a.Config.Mouse,
		ImageProtocol: termimg.Detect(os.Getenv),
		MediaDir:      filepath.Join(a.Config.DataDir, "media"),
		DownloadDir:   a.Config.DownloadDir,
	}
	if p, ok := termimg.ParseProtocol(a.Config.ImageProtocol); ok {
		opts.ImageProtocol = p
	}
	if opts.DownloadDir == "" {
		opts.DownloadDir = downloadsDir()
	}
	debug.SetMemoryLimit(memoryLimit)

	if os.Getenv("WA_DEMO") == "1" {
		demo := newDemoBackend()
		opts.MediaDir = filepath.Join(os.TempDir(), "wa-demo-media")
		return runUI(ctx, demo, opts, os.Stdout, func(post func(whatsapp.Event)) error {
			demo.onEvent(post)
			post(whatsapp.ConnEvent{State: whatsapp.ConnConnected})
			return nil
		})
	}

	if needsMigration(a.Config.DataDir) {
		migrateInChild()
	}

	// whatsmeow logs warnings/errors directly to stdout, which would
	// corrupt the screen Bubble Tea is drawing. Send stdout to a log file
	// and hand Bubble Tea the real terminal explicitly.
	realStdout := os.Stdout
	logPath := filepath.Join(a.Config.DataDir, "tui.log")
	capLogFile(logPath, 2<<20)
	if logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		os.Stdout = logFile
		defer func() {
			os.Stdout = realStdout
			logFile.Close()
		}()
	}
	openDebugLog(a.Config.DataDir)

	dbLog := waLog.Stdout("Database", "WARN", true)
	container, err := store.Open(ctx, a.Config.DataDir, dbLog)
	if err != nil {
		return err
	}
	cs := chatstore.New(a.Config.DataDir)
	ms := msgstore.New(a.Config.DataDir)
	defer ms.Close()

	client, err := whatsapp.New(ctx, container, dbLog, cs, ms)
	if err != nil {
		return err
	}
	if !client.IsLoggedIn() {
		return fmt.Errorf("not logged in: run 'wa login' first")
	}
	client.SetNotifications(a.Config.NotifyEnabled, a.Config.NotifyGroups, a.Config.NotifyShowPreview)
	client.SetBehavior(whatsapp.Behavior{
		SendReadReceipts: a.Config.SendReadReceipts,
		SendPresence:     a.Config.SendPresence,
	})
	go housekeeping(ms, opts.MediaDir, a.Config.HistoryDays)
	defer client.Stop()

	return runUI(ctx, client, opts, realStdout, func(post func(whatsapp.Event)) error {
		client.OnEvent(post)
		if err := client.Start(ctx); err != nil {
			return fmt.Errorf("connecting to WhatsApp: %w", err)
		}
		return nil
	})
}

// runUI runs the Bubble Tea program over a backend. start is called once
// the program can receive events: it hooks the backend's events up to
// post and gets the backend going.
func runUI(ctx context.Context, be Backend, opts Options, out io.Writer, start func(post func(whatsapp.Event)) error) error {
	m := newModel(ctx, be, opts, out)

	// Everything headed for the UI from other goroutines (keys from our
	// own console reader, events from WhatsApp) goes through this queue,
	// so those goroutines never wait on a busy UI and nothing is sent
	// before the program exists.
	inbox := make(chan tea.Msg, 2048)
	done := make(chan struct{})
	defer close(done)
	var overflowed atomic.Bool
	// send waits for room (key presses must never be lost); post drops
	// when flooded and asks for a full reload instead.
	send := func(msg tea.Msg) {
		select {
		case inbox <- msg:
		case <-done:
		}
	}
	post := func(ev whatsapp.Event) {
		dlog("event %T %+v", ev, ev)
		select {
		case inbox <- backendEventMsg{ev}:
		case <-done:
		default:
			overflowed.Store(true)
		}
	}

	progOpts := []tea.ProgramOption{tea.WithAltScreen(), tea.WithOutput(out)}
	// On Windows our own reader replaces Bubble Tea's, which turns every
	// line break of a pasted text into an Enter key press (see
	// internal/conin). Elsewhere Bubble Tea's reader handles pastes fine.
	reader, err := conin.Start(send, conin.Options{Mouse: opts.Mouse})
	if err != nil {
		return fmt.Errorf("reading the console: %w", err)
	}
	if reader != nil {
		defer reader.Stop()
		progOpts = append(progOpts, tea.WithInput(nil))
	} else {
		m.burstGuard = true
		progOpts = append(progOpts, tea.WithReportFocus())
		if opts.Mouse {
			progOpts = append(progOpts, tea.WithMouseCellMotion())
		}
	}
	p := tea.NewProgram(m, progOpts...)

	go func() {
		for {
			select {
			case msg := <-inbox:
				p.Send(msg)
				if overflowed.CompareAndSwap(true, false) {
					p.Send(backendEventMsg{whatsapp.ChatEvent{}}) // reload everything
				}
			case <-done:
				return
			}
		}
	}()

	if err := start(post); err != nil {
		return err
	}
	_, err = p.Run()
	return err
}

// capLogFile starts a log over once it passes limit bytes, so it can't
// grow without bound.
func capLogFile(path string, limit int64) {
	if info, err := os.Stat(path); err == nil && info.Size() > limit {
		_ = os.Rename(path, path+".old")
	}
}

// housekeeping bounds what the TUI keeps on disk: messages past the
// configured age, and media that was only downloaded to be looked at.
func housekeeping(ms *msgstore.Store, mediaDir string, historyDays int) {
	if historyDays > 0 {
		if n, err := ms.Prune(time.Now().AddDate(0, 0, -historyDays), msgstore.MaxPerChat); err != nil {
			dlog("pruning history: %v", err)
		} else if n > 0 {
			dlog("pruned %d old messages", n)
		}
	}
	entries, err := os.ReadDir(mediaDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(mediaDir, e.Name()))
		}
	}
}
