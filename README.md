# wa-cli

```
█     █    █           █████  █       ███ 
█  █  █   █ █         █     █ █        █  
█  █  █  █   █        █       █        █  
█  █  █ █     █ █████ █       █        █  
█  █  █ ███████       █       █        █  
█  █  █ █     █       █     █ █        █  
 ██ ██  █     █        █████  ███████ ███ 
```

[![CI](https://github.com/codebyoketch/wa-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/codebyoketch/wa-cli/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/codebyoketch/wa-cli)](https://github.com/codebyoketch/wa-cli/releases)
[![Go Reference](https://pkg.go.dev/badge/github.com/codebyoketch/wa-cli.svg)](https://pkg.go.dev/github.com/codebyoketch/wa-cli)
[![Go Report Card](https://goreportcard.com/badge/github.com/codebyoketch/wa-cli)](https://goreportcard.com/report/github.com/codebyoketch/wa-cli)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

A WhatsApp client for your terminal, built in Go on top of [whatsmeow](https://github.com/tulir/whatsmeow).

> **Status:** v1.0.0 — stable and cross-platform, with a couple of
> documented known limitations rather than zero known bugs; see
> [ROADMAP.md](./ROADMAP.md) for exactly what's done and what's known
> to still need work.

> **Docs:** browse this repo's docs as a site at
> [codebyoketch.github.io/wa-cli](https://codebyoketch.github.io/wa-cli/),
> or generated API docs at
> [pkg.go.dev/github.com/codebyoketch/wa-cli](https://pkg.go.dev/github.com/codebyoketch/wa-cli).

## Demo

```sh
vhs docs/demo.tape   # generates docs/demo.gif — see the tape file for setup
```

![wa-cli demo](./docs/demo.gif)

## Why

Scriptable, fast, terminal-first WhatsApp: send and receive messages,
manage chats/contacts/groups/media, all from the command line — with
`--json` output for scripting and automation, and a full-screen TUI
(`wa`, no subcommand) for everyday use.

## Install

**Prebuilt binaries** (Linux/macOS/Windows, amd64/arm64) are on the
[Releases page](https://github.com/codebyoketch/wa-cli/releases).
Download the archive for your platform, extract it, and put the `wa`
binary somewhere on your `PATH`.

```sh
# Homebrew (macOS/Linux)
brew install codebyoketch/tap/wa-cli

# Scoop (Windows)
scoop bucket add wa-cli https://github.com/codebyoketch/scoop-bucket
scoop install wa-cli

# Docker
docker run -it -v wa-data:/data ghcr.io/codebyoketch/wa-cli:latest login
```

> **Homebrew on Linux:** if you don't already have `brew`, it isn't
> preinstalled — get it from [brew.sh](https://brew.sh) first
> (`/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"`).
> The installer prints a "Next steps" section with a `PATH` line you
> need to run before `brew` works in your current shell — usually
> `eval "$(/home/linuxbrew/.linuxbrew/bin/brew shellenv bash)"`, added
> to `~/.bashrc` (or your shell's equivalent) so it persists across
> terminals. It also asks for `build-essential` and `bubblewrap` via
> `apt`, needed to install some formulae on Linux.

Or with Go 1.25+ installed:

```sh
go install github.com/codebyoketch/wa-cli@latest
```

Or build from source:

```sh
git clone https://github.com/codebyoketch/wa-cli.git
cd wa-cli
make build   # binary at ./bin/wa
```

For quick local iteration without a build step, use `go run .` in place
of `wa` anywhere below.

## Getting started

```sh
wa login              # scan the QR code with WhatsApp on your phone
wa status             # confirm you're logged in
wa chat list           # sync and list your chats
```

Run `wa` with no subcommand to open the full-screen terminal UI instead
of using individual commands.

## The terminal UI

`wa` opens a two-pane WhatsApp: your chats on the left, the conversation
on the right. Everything stays on your machine; it uses well under
100 MB of memory.

| Where | Key | What it does |
| --- | --- | --- |
| Chat list | type | search chats and contacts (picking a contact starts a new chat) |
| | `↑` `↓` `PgUp` `PgDn` | move; the highlighted chat is previewed without marking it read |
| | `Enter` / `Tab` | open the chat and start typing |
| | `Ctrl+P` `Ctrl+A` `Ctrl+T` `Ctrl+U` | pin, archive, mute, mark unread (synced with your phone) |
| | `Ctrl+R` | show or hide archived chats |
| | `Ctrl+F` | search the messages of every chat |
| Writing | `Enter` | send |
| | `Shift+Enter` | new line (also `Ctrl+Enter`, `Alt+Enter`, `Ctrl+J`) |
| | paste | pasted text keeps its line breaks and stays one message |
| | `@name` | mention a group member |
| | `:name` | pick an emoji, e.g. `:thumbsup` |
| | `Ctrl+O` | attach a file by path — or just drop the file onto the terminal |
| | `Alt+V` | attach the picture on the clipboard, e.g. a screenshot (Windows) |
| | `Ctrl+D` | send the attached file as a document instead |
| | `↑` | select the newest message |
| | `Ctrl+F` / `Ctrl+G` | search this chat / chat and group info |
| A selected message | `Enter` | view a photo full screen, open other media or a link, retry a failed send, otherwise reply |
| | `r` `e` `E` `d` `f` `c` | reply, react, edit, delete (for everyone or just here), forward, copy |
| | `o` / `s` | open with the default app (photos, video, audio, PDFs and office documents; other files are never started, only saved) / save to `Downloads/WhatsApp` |
| Anywhere | `F1` | all keys |
| | `Ctrl+C` | quit |

Also there: delivery ticks (✓ sent, ✓✓ delivered, blue ✓✓ read), "typing…"
and "online / last seen", replies shown with what they quote, reactions
and edits applied to the message they belong to, polls with their tally,
drafts kept per chat, unread counts in the window title, mouse wheel
scrolling and clicking, and older history loaded as you scroll up (asked
from your phone when it isn't stored locally).

**Photos in HD.** `Enter` on a photo opens it full screen at the best
quality the terminal can show: real pixels through Sixel (Windows
Terminal 1.22+ and others), the Kitty protocol or iTerm2's, falling back
to block characters anywhere else (`m` switches mode, `o` opens the
original in your system's image viewer). When someone sends a photo in
HD, the HD version replaces the standard one. Photos you send go out at
their original quality (up to 4096 px on the long side) rather than
recompressed.

Settings. Read receipts and "online" / "typing…" are off by default, so
reading in the terminal tells nobody anything; turn them on to behave
like the official apps:

```sh
wa config set sendReadReceipts true    # senders get blue ticks when you open their chat
wa config set sendPresence true        # show "online" / "typing…" (and see others'; WhatsApp only shares it both ways)
wa config set mouse false              # leave the mouse to the terminal (text selection)
wa config set imageProtocol auto|sixel|kitty|iterm|blocks
wa config set downloadDir <folder>     # where saved media goes
wa config set historyDays 0            # days of messages to keep; 0 = everything
```

`WA_DEMO=1 wa` opens the same interface on made-up chats — nothing is
connected or sent — to try it out.

## Usage

**Chats**
```sh
wa chat list [--no-sync]        # list chats, most recent first
wa chat search <query>          # search chats by name
wa chat info <jid-or-name>      # details for one chat
wa chat open <jid-or-name>      # show local message history
wa chat send <recipient> <message...>
wa chat reply <recipient> <message-ref> <message...>
wa chat forward <from-chat> <message-ref> <to-recipient>
wa chat mute / unmute <chat>    # suppress desktop notifications per chat
```

**Receiving**
```sh
wa watch                        # long-running: print incoming messages live
```

**Contacts**
```sh
wa contact list
wa contact search <query>
wa contact info <name-or-jid>
```

**Groups**
```sh
wa group list
wa group info <name-or-jid>
wa group create <name> <participant...>
wa group add / remove <group> <participant...>
```

**Media**
```sh
wa media send image/video/audio/document/sticker <recipient> <file>
wa media download <chat> <message-ref> [output-path]
wa media list <chat>            # media messages in a chat's local history
```
`image` send/download are verified against a real account; the other
media types are implemented but not yet confirmed end-to-end — see
Phase 8 in [ROADMAP.md](./ROADMAP.md).

**Config**
```sh
wa config get [key]             # print all settings, or one
wa config set <key> <value>     # change and save one setting
wa config edit                  # open config.json in $EDITOR
wa config init                  # write out defaults
```

**Extensions**
```sh
wa extension install <git-url>
wa extension list
wa extension run <name> [-- args...]
wa extension remove <name>
```

**Other**
```sh
wa status                       # login status
wa version
wa completion bash|zsh|fish|powershell
```

### JSON output

Every list/read command above (`chat list/search/info/open`, `contact
list/search/info`, `group list/info`, `media list`, `status`, `extension
list`) supports `--json` for scripting:

```sh
wa chat list --json | jq '.[] | .name'
```

Set it as the default instead of passing the flag every time:

```sh
wa config set jsonOutput true
```

### Notifications

Desktop notifications fire from `wa watch` and the TUI when a new message
arrives (not from one-shot commands). Configurable via `wa config set`:

```sh
wa config set notifyEnabled true|false
wa config set notifyGroups true|false        # group chats opt-in separately from DMs
wa config set notifyShowPreview true|false   # include message text in the notification
wa chat mute <chat>                          # suppress notifications for one chat
```

## Development

```sh
make build   # compile ./bin/wa
make test    # go test ./...
make lint    # golangci-lint run
make fmt     # gofmt -w .
```

## Roadmap

See [ROADMAP.md](./ROADMAP.md) for the full phase-by-phase plan, what's
done, what's in progress, and known issues.

## Architecture

See [ARCHITECTURE.md](./ARCHITECTURE.md) for a map of the codebase —
package layout and how the pieces fit together.

## Examples

See [docs/EXAMPLES.md](./docs/EXAMPLES.md) for worked examples: scripting
with `--json`, a daily unread-chats digest, piping `wa watch`, sending
from a script without hitting an interactive prompt, exporting chat
history, and writing a minimal extension.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md).

## Security

See [SECURITY.md](./SECURITY.md) for how to report vulnerabilities.

## License

[MIT](./LICENSE)
