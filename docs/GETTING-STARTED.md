# WhatsApp in your terminal: getting started

Takes about 2 minutes. Works on Windows, macOS and Linux.

## 1. Install

You need Node.js (any recent version). If `node -v` doesn't print a
version, install it from [nodejs.org](https://nodejs.org) first.

```sh
npm i -g wa-terminal
```

That gives you the `wa` command.

## 2. Link your WhatsApp

```sh
wa login
```

A QR code appears in the terminal. On your phone, open WhatsApp and go to
**Settings → Linked devices → Link a device**, then scan the code.
wa becomes one of your linked devices, just like WhatsApp Web. Your phone
doesn't need to stay online afterwards.

## 3. Chat

```sh
wa
```

The first start loads your chats, which can take a minute.

| Key | Does |
|---|---|
| type | search chats and contacts |
| ↑ ↓ | move through chats |
| Enter | open the chat and start typing |
| Enter (while typing) | send |
| Shift+Enter | new line |
| Ctrl+O | attach a file (or drop a file on the terminal) |
| ↑ (while typing) | pick a message, then **r** reply, **e** react, **f** forward, **d** delete |
| Esc | back to the chat list |
| F1 | every key |
| Ctrl+C | quit |

Read receipts and "online" are **off** by default: people don't see blue
ticks or that you're online. Turn them on with
`wa config set sendReadReceipts true` and `wa config set sendPresence true`.

## Also from the command line

```sh
wa chat list                       # your chats
wa chat send "Mom" "on my way"     # send to a chat by name
wa watch                           # print new messages as they arrive
```

Run `wa --help` for everything else.

## Updating

When a new version is out, wa shows **update** in its top bar. Run the
install command again:

```sh
npm i -g wa-terminal
```

Keep it updated: WhatsApp stops accepting old versions, and then wa shows
"offline".

## Something wrong?

- **Stuck on "offline"**: update (above), then restart wa.
- **"connected elsewhere"**: another wa window is open with the same
  login. Close one.
- **Logged out**: run `wa login` again.
- **Stop using it**: `wa logout`, then `npm rm -g wa-terminal`. You can
  also remove it from your phone under Linked devices.

Bugs and ideas: [github.com/INTERDICTOR1/wa-cli/issues](https://github.com/INTERDICTOR1/wa-cli/issues)

wa is not affiliated with WhatsApp or Meta.
