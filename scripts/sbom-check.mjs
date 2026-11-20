// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
//
// Web SBOM completeness check (D-27). Two modes.
//
//   node scripts/sbom-check.mjs pairs --sbom <spdx.json> --licences <pnpm-licenses.json | ->
//   node scripts/sbom-check.mjs count --web <web.spdx.json> --merged <spdx.json>
//
// pairs compares the set of name@version pairs of the SPDX packages with the set
// of pairs in `pnpm licenses list --prod --json` (an object keyed by licence,
// each value an array of { name, versions, ... }; an entry counts once per
// version). The package the document DESCRIBES (the root, tierplan-web@0.0.0)
// is the project itself and is excluded; it must be exactly one. The check fails
// on any difference, and on an empty side: a count alone would hide a swapped
// package, and an empty input must never pass vacuously. The sets are compared
// as sets, so the order of either input does not matter, and the difference
// lists are printed sorted.
//
// count passes only when the merged document holds at least as many packages
// with a pkg:npm/ purl as the web SBOM has non-root packages (the release gate
// uses it, so the merged SBOM cannot lose the web interface's components).
//
// Exit codes: 0 pass, 1 check failed, 2 usage or unreadable input.
import { readFileSync } from "node:fs";

function usage(message) {
  console.error(message);
  console.error("usage: sbom-check.mjs pairs --sbom <spdx.json> --licences <pnpm-licenses.json|->");
  console.error("       sbom-check.mjs count --web <web.spdx.json> --merged <spdx.json>");
  process.exit(2);
}

function failCheck(message) {
  console.error(`SBOM CHECK: ${message}`);
  process.exit(1);
}

function flag(args, name) {
  const i = args.indexOf(name);
  if (i < 0 || i + 1 >= args.length || args[i + 1].startsWith("--")) {
    usage(`missing ${name}`);
  }
  return args[i + 1];
}

function readJson(path, label) {
  let text;
  try {
    text = readFileSync(path === "-" ? 0 : path, "utf8");
  } catch (err) {
    usage(`cannot read ${label} ${path}: ${err.message}`);
  }
  try {
    return JSON.parse(text);
  } catch (err) {
    usage(`${label} ${path} is not valid JSON: ${err.message}`);
  }
}

// The SPDX packages that are not the described root, and the root itself.
function splitRoot(doc, label) {
  if (!doc || !Array.isArray(doc.packages)) {
    usage(`${label} has no packages array; is it an SPDX JSON document?`);
  }
  const docId = doc.SPDXID ?? "SPDXRef-DOCUMENT";
  const rootIds = new Set(
    (doc.relationships ?? [])
      .filter((r) => r.relationshipType === "DESCRIBES" && r.spdxElementId === docId)
      .map((r) => r.relatedSpdxElement),
  );
  if (rootIds.size !== 1) {
    failCheck(`${label} describes ${rootIds.size} packages; exactly one root is required`);
  }
  const [rootId] = [...rootIds];
  const root = doc.packages.find((p) => p.SPDXID === rootId);
  if (!root) {
    failCheck(`${label} describes ${rootId}, which is not among its packages`);
  }
  return { root, others: doc.packages.filter((p) => p.SPDXID !== rootId) };
}

function sbomPairs(doc, label) {
  const { root, others } = splitRoot(doc, label);
  return { root, pairs: new Set(others.map((p) => `${p.name}@${p.versionInfo}`)), count: others.length };
}

function licencePairs(data) {
  const pairs = new Set();
  if (data && typeof data === "object") {
    for (const list of Object.values(data)) {
      if (!Array.isArray(list)) continue;
      for (const p of list) {
        for (const v of Array.isArray(p.versions) ? p.versions : []) {
          pairs.add(`${p.name}@${v}`);
        }
      }
    }
  }
  return pairs;
}

function onlyIn(a, b) {
  return [...a].filter((x) => !b.has(x)).sort();
}

function runPairs(args) {
  const sbom = sbomPairs(readJson(flag(args, "--sbom"), "SBOM"), "the SBOM");
  const licences = licencePairs(readJson(flag(args, "--licences"), "licences list"));
  if (sbom.pairs.size === 0) failCheck("the SBOM lists no npm package");
  if (licences.size === 0) failCheck("the licences list is empty");

  const extraSbom = onlyIn(sbom.pairs, licences);
  const extraLicences = onlyIn(licences, sbom.pairs);
  for (const pair of extraSbom) console.error(`SBOM CHECK: only in the SBOM: ${pair}`);
  for (const pair of extraLicences) console.error(`SBOM CHECK: only in pnpm licenses: ${pair}`);
  if (extraSbom.length > 0 || extraLicences.length > 0) {
    process.exit(1);
  }
  console.log(
    `sbom-check: ${sbom.pairs.size} npm packages match pnpm licenses (root ${sbom.root.name}@${sbom.root.versionInfo} excluded)`,
  );
}

function runCount(args) {
  const web = sbomPairs(readJson(flag(args, "--web"), "web SBOM"), "the web SBOM");
  const merged = readJson(flag(args, "--merged"), "merged SBOM");
  if (!merged || !Array.isArray(merged.packages)) {
    usage("the merged SBOM has no packages array; is it an SPDX JSON document?");
  }
  const npm = merged.packages.filter((p) =>
    (p.externalRefs ?? []).some((r) => typeof r.referenceLocator === "string" && r.referenceLocator.startsWith("pkg:npm/")),
  ).length;
  if (web.count === 0) failCheck("the web SBOM lists no npm package");
  if (npm < web.count) {
    failCheck(`the merged SBOM lists ${npm} npm packages, fewer than the ${web.count} in the web SBOM`);
  }
  console.log(`sbom-check: merged SBOM lists ${npm} npm packages (web SBOM ${web.count})`);
}

const [mode, ...rest] = process.argv.slice(2);
if (mode === "pairs") runPairs(rest);
else if (mode === "count") runCount(rest);
else usage("unknown or missing mode");
