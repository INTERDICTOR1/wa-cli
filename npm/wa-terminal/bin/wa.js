#!/usr/bin/env node
// Runs the native wa binary that npm installed for this platform (one of
// the @interdictor/wa-tui-<os>-<cpu> packages listed in optionalDependencies).
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");

const pkg = `@interdictor/wa-tui-${process.platform}-${process.arch}`;
const exe = process.platform === "win32" ? "wa.exe" : "wa";

let bin;
try {
  bin = require.resolve(`${pkg}/bin/${exe}`);
} catch {
  console.error(
    `wa-tui: no wa binary for ${process.platform}-${process.arch}.\n` +
      `Reinstall with optional dependencies enabled: npm i -g wa-tui`
  );
  process.exit(1);
}

if (process.platform !== "win32") {
  try {
    fs.accessSync(bin, fs.constants.X_OK);
  } catch {
    try {
      fs.chmodSync(bin, 0o755);
    } catch {}
  }
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(`wa-tui: could not start ${bin}: ${res.error.message}`);
  process.exit(1);
}
if (res.signal) {
  process.kill(process.pid, res.signal);
}
process.exit(res.status ?? 1);
