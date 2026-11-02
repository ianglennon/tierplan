// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//
// Third-party notices generator (D-60, PLAT-05). Writes
// web/public/third-party-notices.json, the list the About page shows:
// [{ name, version, licence }], sorted by name (case-insensitive) then version.
//
//   node scripts/gen-notices.mjs
//
// Sources, each mirroring scripts/licences-check.sh so the notices cover exactly
// what the licence gate checks:
//   1. Go modules the server binary links, on linux/amd64 and linux/arm64
//      (go-licenses for the licence, `go list -m` for the version).
//   2. Production npm packages (`pnpm licenses list --json --prod`), which
//      includes the bundled Geist fonts.
//   3. Static entries for material that is vendored rather than installed.
//
// The output depends only on the dependency versions, never on the machine or
// the clock, so `make gen-check` can diff it.
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const OUT = join(root, "web", "public", "third-party-notices.json");
const GO_LICENSES = "github.com/google/go-licenses/v2@v2.0.1";
const OWN_MODULE = "github.com/ianglennon/tierplan";

// Material that is copied into the repository, so no package manager lists it.
const STATIC_ENTRIES = [
  { name: "shadcn/ui components (vendored in web/src/components/ui)", version: "vendored", licence: "MIT" },
  {
    name: "SecLists xato-net-10-million-passwords (filtered, internal/auth/commonpw.txt)",
    version: "vendored",
    licence: "MIT",
  },
];

// Packages whose attribution must never silently drop out of the list.
const REQUIRED_NPM = ["@fontsource-variable/geist", "@fontsource-variable/geist-mono"];

function run(cmd, args, options = {}) {
  return execFileSync(cmd, args, { cwd: root, encoding: "utf8", maxBuffer: 256 * 1024 * 1024, ...options });
}

// Maps module path to version. A replaced module reports the version it was
// actually built from; the name stays the path the code imports.
function goModuleVersions() {
  const out = run("go", ["list", "-m", "-f", "{{.Path}}\t{{.Version}}\t{{if .Replace}}{{.Replace.Version}}{{end}}", "all"]);
  const versions = new Map();
  for (const line of out.split("\n")) {
    if (line === "") continue;
    const [path, version, replaced] = line.split("\t");
    if (path === OWN_MODULE) continue;
    versions.set(path, replaced || version);
  }
  return versions;
}

// The module that owns a package path: the longest module path that is a prefix.
function ownerOf(pkg, modules) {
  let best = "";
  for (const mod of modules.keys()) {
    if ((pkg === mod || pkg.startsWith(`${mod}/`)) && mod.length > best.length) best = mod;
  }
  return best;
}

// Minimal CSV line split for go-licenses: package,license_url,license_name.
// Neither the package path nor the SPDX name contains a comma.
function parseGoCsv(text) {
  return text
    .split("\n")
    .filter((l) => l !== "")
    .map((l) => {
      const parts = l.split(",");
      return { pkg: parts[0], licence: parts[parts.length - 1] };
    });
}

function goEntries() {
  const modules = goModuleVersions();
  const tools = mkdtempSync(join(tmpdir(), "tierplan-notices-"));
  try {
    // Built for the host once: `go run` with GOARCH set would compile the tool for
    // the target and fail to execute it (see scripts/licences-check.sh).
    run("go", ["install", GO_LICENSES], { env: { ...process.env, GOBIN: tools }, stdio: ["ignore", "ignore", "inherit"] });
    // module -> set of licences, over both release architectures.
    const found = new Map();
    for (const arch of ["amd64", "arm64"]) {
      const csv = run(join(tools, "go-licenses"), ["report", "./cmd/tierplan", "--ignore", OWN_MODULE], {
        env: { ...process.env, CGO_ENABLED: "0", GOOS: "linux", GOARCH: arch },
        stdio: ["ignore", "pipe", "ignore"],
      });
      for (const { pkg, licence } of parseGoCsv(csv)) {
        const mod = ownerOf(pkg, modules);
        if (mod === "") throw new Error(`gen-notices: no Go module owns package ${pkg}`);
        if (!found.has(mod)) found.set(mod, new Set());
        found.get(mod).add(licence);
      }
    }
    return [...found].map(([mod, licences]) => ({
      name: mod,
      version: modules.get(mod),
      licence: [...licences].sort().join(" AND "),
    }));
  } finally {
    rmSync(tools, { recursive: true, force: true });
  }
}

// pnpm prints an object keyed by licence, each value an array of packages that
// each list every installed version.
function npmEntries() {
  const byLicence = JSON.parse(run("pnpm", ["-C", "web", "licenses", "list", "--json", "--prod"]));
  const out = [];
  for (const [licence, packages] of Object.entries(byLicence)) {
    for (const p of packages) {
      for (const version of p.versions) out.push({ name: p.name, version, licence: p.license || licence });
    }
  }
  for (const name of REQUIRED_NPM) {
    if (!out.some((e) => e.name === name)) throw new Error(`gen-notices: ${name} is missing from the pnpm licence report`);
  }
  return out;
}

// Name case-insensitively, then version, then licence. An explicit locale keeps
// the order the same on every machine.
function compare(a, b) {
  return (
    a.name.toLowerCase().localeCompare(b.name.toLowerCase(), "en") ||
    String(a.version).localeCompare(String(b.version), "en") ||
    (a.licence < b.licence ? -1 : a.licence > b.licence ? 1 : 0)
  );
}

const unique = new Map();
for (const e of [...goEntries(), ...npmEntries(), ...STATIC_ENTRIES]) {
  unique.set(`${e.name}\u0000${e.version}\u0000${e.licence}`, { name: e.name, version: e.version, licence: e.licence });
}
const notices = [...unique.values()].sort(compare);
writeFileSync(OUT, `${JSON.stringify(notices, null, 2)}\n`);
console.log(`notices: ${notices.length} entries -> ${OUT.slice(root.length + 1)}`);
