// Builds the npm packages for wa-tui into npm/dist, and optionally packs
// or publishes them.
//
//   node npm/build.mjs 1.1.0             build only
//   node npm/build.mjs 1.1.0 --pack      also make .tgz files (for local testing)
//   node npm/build.mjs 1.1.0 --publish   also publish to npm
//
// One package per platform carries the prebuilt binary (wa-tui-<os>-<cpu>,
// installed only on matching machines via "os"/"cpu"); the main wa-tui
// package lists them all as optionalDependencies and runs the right one.
// Publish from Linux or macOS (CI does): packing on Windows drops the
// executable bit from the Unix binaries.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.dirname(here);
const dist = path.join(here, "dist");

const version = (process.argv[2] || "").replace(/^v/, "");
const pack = process.argv.includes("--pack");
const publish = process.argv.includes("--publish");
if (!/^\d+\.\d+\.\d+(-[\w.]+)?$/.test(version)) {
  console.error("usage: node npm/build.mjs <version> [--pack|--publish]");
  process.exit(2);
}

// [npm os, npm cpu, GOOS, GOARCH]
const targets = [
  ["win32", "x64", "windows", "amd64"],
  ["win32", "arm64", "windows", "arm64"],
  ["darwin", "x64", "darwin", "amd64"],
  ["darwin", "arm64", "darwin", "arm64"],
  ["linux", "x64", "linux", "amd64"],
  ["linux", "arm64", "linux", "arm64"],
];

const main = JSON.parse(fs.readFileSync(path.join(here, "wa-tui", "package.json"), "utf8"));
const run = (cmd, args, opts = {}) => execFileSync(cmd, args, { stdio: "inherit", ...opts });
const git = (...args) => execFileSync("git", args, { cwd: root, encoding: "utf8" }).trim();

const commit = git("rev-parse", "--short", "HEAD");
const date = new Date().toISOString().replace(/\.\d+Z$/, "Z");
const vpkg = "github.com/codebyoketch/wa-cli/internal/version";
const ldflags = `-s -w -X ${vpkg}.Version=${version} -X ${vpkg}.Commit=${commit} -X ${vpkg}.BuildDate=${date}`;

fs.rmSync(dist, { recursive: true, force: true });
const dirs = [];

for (const [os, cpu, goos, goarch] of targets) {
  const name = `wa-tui-${os}-${cpu}`;
  const dir = path.join(dist, name);
  const exe = os === "win32" ? "wa.exe" : "wa";
  fs.mkdirSync(path.join(dir, "bin"), { recursive: true });
  console.log(`building ${name}`);
  run("go", ["build", "-trimpath", "-ldflags", ldflags, "-o", path.join(dir, "bin", exe), "."], {
    cwd: root,
    env: { ...process.env, CGO_ENABLED: "0", GOOS: goos, GOARCH: goarch },
  });
  if (os !== "win32") fs.chmodSync(path.join(dir, "bin", exe), 0o755);
  const pkg = {
    name,
    version,
    description: `The ${os}-${cpu} binary for wa-tui (install wa-tui, not this)`,
    repository: main.repository,
    license: main.license,
    os: [os],
    cpu: [cpu],
    files: ["bin", "LICENSE"],
    preferUnplugged: true,
  };
  fs.writeFileSync(path.join(dir, "package.json"), JSON.stringify(pkg, null, 2) + "\n");
  fs.copyFileSync(path.join(root, "LICENSE"), path.join(dir, "LICENSE"));
  main.optionalDependencies[name] = version;
  dirs.push(dir);
}

const mainDir = path.join(dist, "wa-tui");
fs.cpSync(path.join(here, "wa-tui"), mainDir, { recursive: true });
fs.copyFileSync(path.join(root, "LICENSE"), path.join(mainDir, "LICENSE"));
main.version = version;
fs.writeFileSync(path.join(mainDir, "package.json"), JSON.stringify(main, null, 2) + "\n");
dirs.push(mainDir); // last: its dependencies must be published first

const npm = process.platform === "win32" ? "npm.cmd" : "npm";
for (const dir of dirs) {
  if (pack) run(npm, ["pack", "--pack-destination", dist], { cwd: dir, shell: process.platform === "win32" });
  if (publish) run(npm, ["publish", "--access", "public"], { cwd: dir, shell: process.platform === "win32" });
}
console.log(`done: ${dirs.length} packages in ${dist}`);
