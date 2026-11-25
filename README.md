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

[![CI](https://github.com/INTERDICTOR1/wa-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/INTERDICTOR1/wa-cli/actions/workflows/ci.yml)
[![npm](https://img.shields.io/npm/v/wa-terminal)](https://www.npmjs.com/package/wa-terminal)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

WhatsApp in your terminal. You get a full chat app that runs in the terminal, and a set of commands you can script.

This started as [codebyoketch/wa-cli](https://github.com/codebyoketch/wa-cli). That project isn't updated anymore, and since Nov 25, 2026 WhatsApp refuses to connect to its last release, so I picked it up and I'm maintaining it here. It's built in Go on top of [whatsmeow](https://github.com/tulir/whatsmeow).

![wa-cli demo](./docs/demo.gif)

## Install

You need Node.js. Then:

```sh
npm i -g wa-terminal
wa login
wa
```

`wa login` shows a QR code. Scan it from WhatsApp on your phone under Settings, Linked devices, Link a device. After that, `wa` opens your chats. It works on Windows, Mac and Linux, and npm downloads a ready-made binary, so there's nothing to compile.

If you're new to it, read the [getting started guide](docs/GETTING-STARTED.md). It takes two minutes.

To update, run `npm i -g wa-terminal` again. wa tells you in its top bar when there's a new version. Don't sit on old versions for long, because WhatsApp eventually stops accepting them and wa will just say offline.

If you'd rather build it yourself, you need Go 1.26 or newer:

```sh
git clone https://github.com/INTERDICTOR1/wa-cli.git
cd wa-cli
make build   # puts the binary at ./bin/wa
```

## The chat app

Run `wa` with nothing after it. Your chats are on the left and the open conversation is on the right. Everything stays on your machine, and the top right corner shows how much RAM it's using.

It's also a lot lighter than WhatsApp Web. On my laptop, the WhatsApp Web tab in Firefox sits around 1 GB of RAM and 120 to 140% CPU. wa sits around 50 MB, peaks at about 100 MB, and uses about 2% CPU while it waits for messages.

Start typing to search your chats and contacts. Picking a contact starts a new chat. The arrow keys move through the list, and the highlighted chat is previewed without being marked as read. Enter or Tab opens it.

While writing, Enter sends and Shift+Enter adds a new line. Pasted text keeps its line breaks and goes out as one message. Type @ and a name to mention someone in a group, or : and a name to pick an emoji, like :thumbsup. To attach a file, press Ctrl+O or drag it onto the terminal. Alt+V attaches whatever picture is on your clipboard, like a screenshot (Windows only for now). Ctrl+D sends the attachment as a document instead.

Press the up arrow to select a message. Then r replies, e reacts, E edits your own message, d deletes it, f forwards it and c copies the text. Enter opens a photo full screen or opens a link. o opens media with your default app and s saves it to Downloads/WhatsApp. Only safe file types get opened. Anything else is saved, never run.

A few more keys: Ctrl+P pins a chat, Ctrl+A archives it, Ctrl+T mutes it and Ctrl+U marks it unread, and all of that syncs with your phone. Ctrl+R shows archived chats. Ctrl+F searches messages, and Ctrl+G shows chat or group info. F1 lists every key, and Ctrl+C quits.

You also get delivery ticks, typing and last seen, replies with the message they quote, reactions, edits, polls, drafts saved per chat, mouse scrolling and clicking, and older messages loading as you scroll up.

Photos open at the best quality your terminal can show. Windows Terminal, Kitty and iTerm2 show real pixels. Other terminals fall back to block characters. Press m to switch the drawing mode, or o to open the original in your image viewer. Photos you send keep their original quality instead of being recompressed.

If you just want to look around first, `WA_DEMO=1 wa` opens the app with made-up chats. Nothing connects and nothing gets sent.

## Settings

Read receipts and online status are off by default. Nobody sees blue ticks or that you're online just because you have it open. Turn them on if you want it to behave like the normal apps:

```sh
wa config set sendReadReceipts true    # people see blue ticks when you open their chat
wa config set sendPresence true        # show online and typing, and see other people's
wa config set mouse false              # leave the mouse to the terminal, for selecting text
wa config set imageProtocol auto       # or sixel, kitty, iterm, blocks
wa config set downloadDir <folder>     # where saved media goes
wa config set historyDays 0            # how many days of messages to keep, 0 keeps everything
```

## Commands

You don't have to use the chat app. Everything also works as plain commands, which is handy for scripts.

```sh
wa chat list                    # your chats, newest first
wa chat search <query>
wa chat info <chat>
wa chat open <chat>             # recent messages
wa chat send <chat> <message>
wa chat reply <chat> <message-ref> <message>
wa chat forward <from-chat> <message-ref> <to-chat>
wa chat mute <chat>             # and unmute

wa watch                        # print new messages as they arrive

wa contact list                 # also search and info
wa group list                   # also info, create, add and remove

wa media send image <chat> <file>   # also video, audio, document and sticker
wa media download <chat> <message-ref> [path]
wa media list <chat>

wa config get                   # also set, edit and init
wa extension install <git-url>  # also list, run and remove
wa status
wa version
wa completion bash              # or zsh, fish, powershell
```

Sending and downloading images is tested on a real account. The other media types are built but haven't been fully tested yet.

Anything that lists or reads data can print JSON with `--json`, so you can pipe it into other tools:

```sh
wa chat list --json | jq '.[] | .name'
wa config set jsonOutput true   # make JSON the default
```

`wa watch` and the chat app can show desktop notifications for new messages. You can turn them off, leave out groups, or hide the message text:

```sh
wa config set notifyEnabled false
wa config set notifyGroups false
wa config set notifyShowPreview false
```

There are more worked examples in [docs/EXAMPLES.md](docs/EXAMPLES.md), like a daily digest of unread chats and sending from a script.

## Working on it

```sh
make build
make test
make lint
make fmt
```

[ARCHITECTURE.md](ARCHITECTURE.md) explains how the code is laid out, and [ROADMAP.md](ROADMAP.md) covers what's done and what isn't. If you want to help, read [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues the way [SECURITY.md](SECURITY.md) describes.

Releases go out by pushing a version tag, and GitHub publishes it to npm.

This isn't affiliated with WhatsApp or Meta. MIT licensed, see [LICENSE](LICENSE).
