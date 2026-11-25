# Getting started

This is WhatsApp in your terminal. It started as codebyoketch/wa-cli, which isn't updated anymore, so I picked it up and I'm maintaining it here. Setup takes a couple of minutes and works on Windows, Mac and Linux.

## Install

You need Node.js. If `node -v` doesn't print a version, install it from [nodejs.org](https://nodejs.org) first. Then run:

```sh
npm i -g wa-terminal
```

That gives you the `wa` command.

## Link your WhatsApp

Run `wa login` and a QR code shows up in the terminal. On your phone, open WhatsApp, go to Settings, then Linked devices, then Link a device, and scan it. wa is now linked the same way WhatsApp Web is, so your phone doesn't have to stay online.

## Use it

Run `wa` to open your chats. The first start takes a minute while it loads everything.

Start typing to search your chats, use the arrow keys to move, and press Enter to open one. Type your message and press Enter to send it, or Shift+Enter for a new line. To attach a file, press Ctrl+O or just drag the file onto the terminal. Press the up arrow while typing to pick a message, then r to reply, e to react, f to forward or d to delete. Esc takes you back to the chat list, F1 shows every key, and Ctrl+C quits.

Read receipts and online status are off by default, so people won't see blue ticks or that you're online. If you want them, run `wa config set sendReadReceipts true` and `wa config set sendPresence true`.

You can also use it without the full screen view. `wa chat list` shows your chats, `wa chat send "Mom" "on my way"` sends a message, and `wa watch` prints new messages as they come in. `wa --help` lists everything else.

## Updating

When there's a new version, wa shows it in the top bar. Run `npm i -g wa-terminal` again to update. Don't skip updates for long, because WhatsApp stops accepting old versions and wa will just say offline.

## If something's off

If it's stuck on offline, update and restart it. If it says connected elsewhere, you have another wa open with the same login, so close one. If it says logged out, run `wa login` again.

To stop using it, run `wa logout` and then `npm rm -g wa-terminal`. You can also remove it from your phone under Linked devices.

If you find a bug or want something added, open an issue at [github.com/INTERDICTOR1/wa-cli/issues](https://github.com/INTERDICTOR1/wa-cli/issues).

This isn't affiliated with WhatsApp or Meta.
