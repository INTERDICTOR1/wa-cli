# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to follow [Semantic Versioning](https://semver.org/)
from v1.0.0 onward.

## [Unreleased]

### TUI overhaul

Fixed

- Pasting text with line breaks sent the first line at once and each
  other line as its own message. On Windows every pasted line break
  reached the TUI as an Enter key press. The TUI now reads the Windows
  console itself (`internal/conin`) and recognises pastes; the message
  box is multi-line, and only a deliberate Enter sends. Verified by an
  end-to-end test that pastes into the real program inside a pseudo
  console.
- HD photos were never shown: the HD version arrives as a second,
  linked message and was displayed as a stray "[associatedChild]" line.
  It now replaces the standard-quality photo.
- Reactions, edits, deletions, poll votes and album notices each showed
  up as their own line ("[reaction 👍]", "[edited] …") and counted as
  unread messages, blanked the chat's preview and fired notifications.
  They are now applied to the message they belong to.
- The same person could appear twice in the chat list (once under their
  phone number, once under WhatsApp's newer privacy ID). Such chats are
  merged.
- A send that failed part-way could be retried under a new message ID
  and arrive twice. Retries now reuse the ID, and only happen when the
  connection wasn't ready.
- The chat jumped to the bottom whenever anything arrived or a name
  changed, even while reading older messages. It now keeps its place and
  shows how many messages arrived below.
- Messages could be stored twice; a renamed group kept its old name; a
  chat preview could be cut in the middle of a character; an error in
  the status line stayed forever; text from a message could inject
  terminal escape sequences.
- Two `wa` processes (e.g. `wa chat send` while the TUI is open)
  overwrote each other's history.

Added

- Replies, reactions, editing, deleting (for everyone or locally),
  forwarding, copying; delivery/read ticks; "typing…" and online/last
  seen; read receipts; pin, archive, mute and mark-unread synced with
  the phone; drafts per chat; searching messages; starting a chat from a
  contact; polls with tallies; day separators and an unread divider.
- Sending files from the TUI (drop a file on the terminal, `Ctrl+O`, or
  `Alt+V` for a screenshot on the clipboard). Photos go out at original
  quality; large files are uploaded from disk as a stream.
- Full-screen photo viewer using Sixel, the Kitty protocol or iTerm2
  inline images, with a block-character fallback (`internal/termimg`);
  opening photos, videos, audio, PDFs and office documents in the
  system's default app; saving any attachment.
- `:name` emoji completion, mouse wheel and click support, a help
  overlay (`F1`), unread count in the window title.
- Whole history kept (no 48-hour cap) and paged in as you scroll, with
  older messages requested from the phone on demand.
- Config: `sendReadReceipts` and `sendPresence` (both off unless you
  turn them on, so reading in the TUI tells nobody anything), `mouse`,
  `imageProtocol`, `downloadDir`, `historyDays`.
- `WA_DEMO=1 wa` runs the interface on made-up data.

Changed

- Chats and messages moved from two JSON files, rewritten in full on
  every message, to one SQLite file (`wa.db`, `internal/localdb`). The
  old files are imported once and kept as `*.migrated`.
- The TUI talks to WhatsApp through a small interface
  (`internal/tui/backend.go`) and typed events, so it can be tested
  without an account.
- The one-time import runs in a short-lived helper process (`wa
  migrate-store`, started automatically), so the long-running TUI never
  carries the memory it takes.
- New dependency: `golang.org/x/image` (WebP stickers, scaling).

Security

Three flaws in the first version of this overhaul, found by review right
after it was pushed:

- Opening an attachment handed whatever was downloaded to the system's
  default app. A document is named by its sender, so "opening"
  `invoice.pdf.exe` would have run it. Now only types that are shown or
  played are opened (a fixed list); anything else is saved and never
  started. A file's extension comes from the type its sender claims only
  when that type is on a fixed list of harmless ones, and downloads carry
  Windows' "mark of the web", as files from a browser do.
- In a group, a "delete for everyone" aimed at someone else's message
  was applied whenever the member list wasn't loaded, so any member could
  erase others' messages from your history. It is now applied only once
  its sender is confirmed to be an admin; the member list is fetched for
  that if needed, and a claim that can't be checked deletes nothing.
- A "can only be seen on your phone" placeholder was replaced by any
  later message with the same ID. Another group member could reuse the ID
  and have their words shown under the first sender's name. Only the
  placeholder's own sender can replace it now.

Tests

- The `internal/app`, `internal/config` and `internal/extension` tests
  only redirected `XDG_CONFIG_HOME`, which Windows ignores - on Windows
  they wrote into the real `%APPDATA%\wa`, overwriting `config.json`.
  They now redirect the variable each OS actually reads. With that and a
  missing `Close` in the store tests fixed, the suites that always
  failed on Windows pass.

### Added

- Phase 0: project scaffold — MIT license, README, CONTRIBUTING,
  CODE_OF_CONDUCT, SECURITY, this changelog, GitHub Actions CI, golangci-lint
  config, Makefile.
- Phase 1: core architecture — `internal/config`, `internal/logger`,
  `internal/version`, `internal/errors`, `internal/app`, plus a minimal
  dependency-free `internal/cli` command router.
- `wa --help`, `wa version`, `wa config get`, `wa config init`.
