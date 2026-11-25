// Builds the npm packages for wa-tui into npm/dist, and optionally packs
// or publishes them.
//
//   node npm/build.mjs 1.1.0             build only
//   node npm/build.mjs 1.1.0 --pack      also make .tgz files (for local testing)
//   node npm/build.mjs 1.1.0 --publish   also publish to npm
//
// One package per platform carries the prebuilt binary,
// @interdictor/wa-tui-<os>-<cpu>, installed only on matching machines
// via "os"/"cpu". They are scoped because npm's spam filter rejects a
// batch of similar new unscoped names. The main wa-tui package lists them
// all as optionalDependencies and runs the right one. Publishing skips
// versions already on npm, so a run that stopped halfway can be repeated.
// The .tgz files are written here rather than by `npm pack`, so the Unix
// binaries keep their executable bit even when built on Windows.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import zlib from "node:zlib";
import path from "node:path";
import { fileURLToPath } from "node:url";

// The npm scope of the platform packages; bin/wa.js names it too.
const scope = "@interdictor";

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
  const name = `${scope}/wa-tui-${os}-${cpu}`;
  const dir = path.join(dist, `wa-tui-${os}-${cpu}`);
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
// Only these files: anything else that lands in npm/wa-tui (an .npmrc,
// an .env) must never be published.
for (const f of ["bin/wa.js", "README.md"]) {
  fs.mkdirSync(path.dirname(path.join(mainDir, f)), { recursive: true });
  fs.copyFileSync(path.join(here, "wa-tui", f), path.join(mainDir, f));
}
fs.copyFileSync(path.join(root, "LICENSE"), path.join(mainDir, "LICENSE"));
main.version = version;
fs.writeFileSync(path.join(mainDir, "package.json"), JSON.stringify(main, null, 2) + "\n");
dirs.push(mainDir); // last: its dependencies must be published first

for (const dir of dirs) {
  if (!pack && !publish) continue;
  const { name } = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8"));
  const tgz = path.join(dist, `${path.basename(dir)}-${version}.tgz`);
  fs.writeFileSync(tgz, tarball(dir));
  console.log(`packed ${path.basename(tgz)}`);
  if (publish && published(name)) {
    console.log(`${name}@${version} is already on npm, skipping`);
  } else if (publish) {
    // Through the shell on Windows, where npm is a .cmd script.
    const win = process.platform === "win32";
    run(win ? `npm publish "${tgz}" --access public` : "npm", win ? [] : ["publish", tgz, "--access", "public"], { shell: win });
  }
}
console.log(`done: ${dirs.length} packages in ${dist}`);

// published reports whether this version of name is already on npm, so a
// publish that stopped halfway can simply be run again.
function published(name) {
  try {
    const out = execFileSync(`npm view ${name}@${version} version`, { encoding: "utf8", shell: true, stdio: ["ignore", "pipe", "ignore"] });
    return out.trim() === version;
  } catch {
    return false;
  }
}

// tarball returns dir as an npm package .tgz: every file under
// "package/", files in bin/ executable, fixed owner and date (as npm does).
function tarball(dir) {
  const files = [];
  const walk = (rel) => {
    for (const e of fs.readdirSync(path.join(dir, rel), { withFileTypes: true })) {
      const r = rel ? `${rel}/${e.name}` : e.name;
      if (e.isDirectory()) walk(r);
      else files.push(r);
    }
  };
  walk("");
  files.sort();
  const blocks = [];
  for (const rel of files) {
    const data = fs.readFileSync(path.join(dir, rel));
    const name = `package/${rel}`;
    if (Buffer.byteLength(name) > 100) throw new Error(`path too long for tar: ${name}`);
    const h = Buffer.alloc(512);
    const field = (off, len, str) => h.write(str, off, len, "ascii");
    const octal = (off, len, n) => field(off, len, n.toString(8).padStart(len - 1, "0") + "\0");
    field(0, 100, name);
    octal(100, 8, rel.startsWith("bin/") ? 0o755 : 0o644);
    octal(108, 8, 0);
    octal(116, 8, 0);
    octal(124, 12, data.length);
    octal(136, 12, 499162500); // 1985-10-26, npm's fixed mtime
    field(148, 8, "        ");
    field(156, 1, "0");
    field(257, 6, "ustar\0");
    field(263, 2, "00");
    let sum = 0;
    for (const b of h) sum += b;
    field(148, 8, sum.toString(8).padStart(6, "0") + "\0 ");
    blocks.push(h, data, Buffer.alloc((512 - (data.length % 512)) % 512));
  }
  blocks.push(Buffer.alloc(1024));
  return zlib.gzipSync(Buffer.concat(blocks), { level: 9 });
}
