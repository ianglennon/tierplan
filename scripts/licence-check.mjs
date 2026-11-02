// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//
// Dependency licence gate (D-70). One allowlist checker for both ecosystems.
//
//   node scripts/licence-check.mjs --format go-csv    < go-licenses-report.csv
//   node scripts/licence-check.mjs --format pnpm-json < pnpm-licenses.json
//
// go-csv is `go-licenses report` output: package,license_url,license_name.
// pnpm-json is `pnpm licenses list --json --prod`: an object keyed by licence,
// each value an array of { name, versions, license, ... }.
//
// A licence passes when it is on the allowlist. SPDX expressions are evaluated:
// OR passes if any branch is allowed, AND needs every branch. Anything else
// (GPL-2.0-only, SSPL, BUSL, Commons Clause, classifier names such as GPL-3.0
// that cannot be resolved to an or-later variant, Unknown) fails unless
// licence-exceptions.txt has a reviewed line:
//
//   <go|npm> <package-or-module-prefix> <licence>  # reason
//
// Exits 1 and prints one `LICENCE FAIL: <ecosystem> <package> <licence>` line
// per offender.
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const ALLOWED = new Set([
  "MIT",
  "BSD-2-Clause",
  "BSD-3-Clause",
  "ISC",
  "Apache-2.0",
  "MPL-2.0",
  "0BSD",
  "Unlicense",
  "CC0-1.0",
  "Zlib",
  "LGPL-3.0-or-later",
  "GPL-3.0-or-later",
]);

const here = dirname(fileURLToPath(import.meta.url));
const EXCEPTIONS_FILE = process.env.LICENCE_EXCEPTIONS ?? join(here, "..", "licence-exceptions.txt");

function fail(message) {
  console.error(message);
  process.exit(2);
}

function parseArgs(argv) {
  const i = argv.indexOf("--format");
  const format = i >= 0 ? argv[i + 1] : undefined;
  if (format !== "go-csv" && format !== "pnpm-json") {
    fail("usage: licence-check.mjs --format go-csv|pnpm-json < input");
  }
  return format;
}

// Tokenise an SPDX expression into identifiers, parentheses and AND/OR/WITH.
function tokenise(expr) {
  return expr.match(/\(|\)|[^\s()]+/g) ?? [];
}

// Evaluate an SPDX expression. AND binds tighter than OR. `X WITH exception`
// is treated as unknown (not allowed) because exceptions change the grant.
function evaluate(expr) {
  const tokens = tokenise(expr);
  let pos = 0;
  const peek = () => tokens[pos];
  const next = () => tokens[pos++];

  function parseOr() {
    let result = parseAnd();
    while (peek() === "OR") {
      next();
      const rhs = parseAnd();
      result = result || rhs;
    }
    return result;
  }
  function parseAnd() {
    let result = parseAtom();
    while (peek() === "AND") {
      next();
      const rhs = parseAtom();
      result = result && rhs;
    }
    return result;
  }
  function parseAtom() {
    const t = next();
    if (t === undefined) return false;
    if (t === "(") {
      const v = parseOr();
      if (peek() === ")") next();
      return v;
    }
    if (peek() === "WITH") {
      next();
      next();
      return false;
    }
    // "GPL-3.0+" / "LGPL-3.0+" are the deprecated spellings of or-later.
    const id = t.replace(/^(L?GPL-3\.0)\+$/, "$1-or-later");
    return ALLOWED.has(id);
  }

  const verdict = parseOr();
  return pos === tokens.length ? verdict : false;
}

function loadExceptions() {
  let text;
  try {
    text = readFileSync(EXCEPTIONS_FILE, "utf8");
  } catch {
    return [];
  }
  const out = [];
  for (const raw of text.split("\n")) {
    const line = raw.replace(/#.*$/, "").trim();
    if (!line) continue;
    const [ecosystem, prefix, ...licence] = line.split(/\s+/);
    if (!ecosystem || !prefix || licence.length === 0) {
      fail(`licence-exceptions.txt: malformed line: ${raw}`);
    }
    if (ecosystem !== "go" && ecosystem !== "npm") {
      fail(`licence-exceptions.txt: ecosystem must be go or npm: ${raw}`);
    }
    out.push({ ecosystem, prefix, licence: licence.join(" ") });
  }
  return out;
}

// Prefix match on a path boundary, so "argparse" does not cover "argparse-evil".
function prefixMatches(prefix, name) {
  return name === prefix || name.startsWith(`${prefix}/`);
}

function parseCsv(text) {
  const rows = [];
  for (const line of text.split("\n")) {
    if (!line.trim()) continue;
    const fields = [];
    let cur = "";
    let quoted = false;
    for (const ch of line) {
      if (ch === '"') quoted = !quoted;
      else if (ch === "," && !quoted) {
        fields.push(cur);
        cur = "";
      } else cur += ch;
    }
    fields.push(cur);
    rows.push(fields);
  }
  return rows;
}

function readPackages(format, text) {
  if (format === "go-csv") {
    return parseCsv(text).map(([name, , licence]) => ({
      ecosystem: "go",
      name,
      licence: licence ?? "Unknown",
    }));
  }
  let data;
  try {
    data = JSON.parse(text);
  } catch (err) {
    fail(`licence-check: input is not valid JSON: ${err.message}`);
  }
  const pkgs = [];
  for (const [group, list] of Object.entries(data)) {
    for (const p of list) {
      pkgs.push({ ecosystem: "npm", name: p.name, licence: p.license ?? group });
    }
  }
  return pkgs;
}

const format = parseArgs(process.argv.slice(2));
const packages = readPackages(format, readFileSync(0, "utf8"));
if (packages.length === 0) {
  // An empty report means the upstream command produced nothing; do not pass vacuously.
  console.error(`LICENCE FAIL: ${format === "go-csv" ? "go" : "npm"} (no packages read) empty input`);
  process.exit(1);
}

const exceptions = loadExceptions();
let failed = 0;
for (const pkg of packages) {
  if (evaluate(pkg.licence)) continue;
  const excepted = exceptions.some(
    (e) => e.ecosystem === pkg.ecosystem && e.licence === pkg.licence && prefixMatches(e.prefix, pkg.name),
  );
  if (excepted) continue;
  console.log(`LICENCE FAIL: ${pkg.ecosystem} ${pkg.name} ${pkg.licence}`);
  failed = 1;
}
process.exit(failed);
