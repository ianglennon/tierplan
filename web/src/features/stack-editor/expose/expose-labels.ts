// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import {
  type Document,
  type DocumentOptions,
  isAlias,
  isCollection,
  isMap,
  isScalar,
  isSeq,
  type Node,
  Pair,
  type ParseOptions,
  parseDocument,
  Scalar,
  type ScalarTag,
  type SchemaOptions,
  YAMLMap,
  YAMLSeq,
} from "yaml";

// The label editor of the Expose panel (D-18, D-19, A-08). It is pure: text in, text out, through
// the `yaml` Document API, so comments, key order and every node it does not touch survive. It
// writes only the tierplan.ingress.* entries of one service, in the place the stack's target reads
// them (deploy.labels on a swarm, labels on a standalone host) and in the form it found (a map stays
// a map, a list stays a list). It never validates a hostname, a path or a scheme: that is the
// server's check (D-19), and internal/ingress/labels.go is the grammar these keys come from. The
// golden file internal/stacks/testdata/expose-golden.json ties the two together.
//
// A value the editor cannot rewrite safely is locked: an alias, a value under a merge key, a
// ${VARIABLE}. A locked value is read and shown, and never changed (Pitfall 9).

export type ExposeTarget = "swarm" | "standalone";

export type RouteField = "host" | "port" | "path" | "scheme" | "strip";

export type Lock = "variable" | "anchor";

export interface ExposeRoute {
  // "default" for the short form, tierplan.ingress.host.
  name: string;
  host: string;
  port: string;
  // As written; "" when the route has no path label.
  path: string;
  scheme: "http" | "https";
  strip: boolean;
  locked: Partial<Record<RouteField, Lock>>;
}

export interface ExposeRead {
  // Every service of the file, in file order.
  services: string[];
  routes: ExposeRoute[];
  // The form of the block the target reads: a map, a list, or no block yet.
  form: "map" | "list" | "none";
  // Ingress labels sit only in the block the target does not read.
  wrongPlace: boolean;
  // Ingress labels sit in both blocks, so the unused copy is ignored.
  bothPlaces: boolean;
  // The compose file is not valid YAML, so nothing could be read.
  parseError: boolean;
  // The service or its block comes from an anchor, an alias or a merge key, so nothing is written.
  blockLocked: boolean;
}

// The prefix of every ingress label (internal/ingress LabelPrefix).
const PREFIX = "tierplan.ingress.";
// The name of the route the short form declares; the named form may not use it.
const DEFAULT_ROUTE = "default";
const FIELDS: readonly RouteField[] = ["host", "port", "path", "scheme", "strip"];
// A route name, as in internal/ingress/labels.go.
const NAME_RE = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/;
// Compose reads ${NAME} and $NAME as a variable.
const VARIABLE_RE = /\$[{A-Za-z_]/;

function isField(s: string): s is RouteField {
  return (FIELDS as readonly string[]).includes(s);
}

// labelBlockPath is where the target reads a service's labels.
export function labelBlockPath(service: string, target: ExposeTarget): string[] {
  return target === "swarm"
    ? ["services", service, "deploy", "labels"]
    : ["services", service, "labels"];
}

// ---- reading the document ----

type Block = YAMLMap | YAMLSeq;
type Where = "deploy" | "plain";

function whereOf(target: ExposeTarget): Where {
  return target === "swarm" ? "deploy" : "plain";
}

function otherWhere(where: Where): Where {
  return where === "deploy" ? "plain" : "deploy";
}

// A plain integer written with a leading zero (mode: 0440, 007, 08). The core schema reads it as a
// decimal number and writes it back without the zeros, and compose reads such a value as an octal
// mode or an identifier, so the editor keeps the text it was given: the node holds the source string
// and is written plain. A node the editor makes itself has no plain type and is quoted.
const RAW_INT_RE = /^[-+]?0[0-9]+$/;
const rawInt: ScalarTag = {
  tag: "tag:yaml.org,2002:int",
  default: true,
  test: RAW_INT_RE,
  identify: (v) => typeof v === "string" && RAW_INT_RE.test(v),
  resolve: (str) => new Scalar(str),
  stringify: (item) => {
    const v = String((item as Scalar).value);
    if ((item as Scalar).type === Scalar.PLAIN) return v;
    return (item as Scalar).type === Scalar.QUOTE_SINGLE ? `'${v}'` : `"${v}"`;
  },
};
const PARSE: ParseOptions & DocumentOptions & SchemaOptions = {
  customTags: (tags) => [rawInt, ...tags],
};

function load(text: string): Document.Parsed | null {
  const doc = parseDocument(text, PARSE);
  return doc.errors.length > 0 ? null : doc;
}

function keyOf(pair: Pair): string | null {
  return isScalar(pair.key) && pair.key.value != null ? String(pair.key.value) : null;
}

function findPair(map: YAMLMap, key: string): Pair | undefined {
  return map.items.find((p) => keyOf(p) === key);
}

function hasMerge(map: YAMLMap): boolean {
  return map.items.some((p) => keyOf(p) === "<<");
}

function isNullNode(node: unknown): boolean {
  return node == null || (isScalar(node) && node.value == null);
}

function servicesOf(doc: Document.Parsed): YAMLMap | null {
  if (!isMap(doc.contents)) return null;
  const pair = findPair(doc.contents, "services");
  return pair && isMap(pair.value) ? pair.value : null;
}

// serviceOf is the map of one service, or null when the file has none by that name or it is not a
// plain map (an alias, say).
function serviceOf(doc: Document.Parsed, service: string): YAMLMap | null {
  const services = servicesOf(doc);
  const pair = services ? findPair(services, service) : undefined;
  return pair && isMap(pair.value) ? pair.value : null;
}

type Child =
  | { state: "absent"; pair?: Pair }
  | { state: "locked" }
  | { state: "found"; pair: Pair; node: Node };

// child looks one key up in a map. A key that is absent from a map with a merge key may come from
// the merge, so it is locked rather than absent: writing a key beside it would hide what it holds.
function child(map: YAMLMap, key: string): Child {
  const pair = findPair(map, key);
  if (!pair) return hasMerge(map) ? { state: "locked" } : { state: "absent" };
  if (isNullNode(pair.value)) return { state: "absent", pair };
  if (isAlias(pair.value) || !isCollection(pair.value)) return { state: "locked" };
  return { state: "found", pair, node: pair.value as Node };
}

type BlockRef = { state: "absent" } | { state: "locked" } | { state: "found"; block: Block };

// locate finds the labels block of a service in one of the two places.
function locate(svc: YAMLMap, where: Where): BlockRef {
  let holder = svc;
  if (where === "deploy") {
    const d = child(svc, "deploy");
    if (d.state !== "found") return { state: d.state };
    if (!isMap(d.node)) return { state: "locked" };
    holder = d.node;
  }
  const l = child(holder, "labels");
  if (l.state !== "found") return { state: l.state };
  return isMap(l.node) || isSeq(l.node) ? { state: "found", block: l.node } : { state: "locked" };
}

// ---- the entries of a block ----

interface Entry {
  key: string;
  value: string;
  lock: Lock | null;
  // False for an entry that comes in through a merge key: it has no node of its own to change.
  direct: boolean;
  // The pair of a map, or the scalar of a list, when the entry is direct.
  ref: Pair | Scalar | null;
  // The entry's value node carries an anchor, so deleting it would orphan an alias.
  anchored: boolean;
}

function textOf(doc: Document.Parsed, node: unknown): string {
  if (isAlias(node)) {
    const target = node.resolve(doc);
    return target ? textOf(doc, target) : "";
  }
  return isScalar(node) && node.value != null ? String(node.value) : "";
}

function anchorOf(node: unknown): boolean {
  return isScalar(node) || isCollection(node) ? Boolean(node.anchor) : false;
}

// mergedMaps are the maps a merge key pulls in, in the order that wins.
function mergedMaps(doc: Document.Parsed, value: unknown): YAMLMap[] {
  const out: YAMLMap[] = [];
  const add = (node: unknown) => {
    const resolved = isAlias(node) ? node.resolve(doc) : node;
    if (isMap(resolved)) out.push(resolved);
  };
  if (isSeq(value)) for (const item of value.items) add(item);
  else add(value);
  return out;
}

function entriesOf(doc: Document.Parsed, block: Block): Entry[] {
  const out: Entry[] = [];
  if (isSeq(block)) {
    for (const item of block.items) {
      if (!isScalar(item) || typeof item.value !== "string") continue;
      const at = item.value.indexOf("=");
      const value = at < 0 ? "" : item.value.slice(at + 1);
      out.push({
        key: at < 0 ? item.value : item.value.slice(0, at),
        value,
        lock: VARIABLE_RE.test(value) ? "variable" : null,
        direct: true,
        ref: item,
        anchored: anchorOf(item),
      });
    }
    return out;
  }
  const direct = new Set<string>();
  for (const pair of block.items) {
    const k = keyOf(pair);
    if (k !== null) direct.add(k);
  }
  for (const pair of block.items) {
    const k = keyOf(pair);
    if (k === null) continue;
    if (k === "<<") {
      for (const merged of mergedMaps(doc, pair.value)) {
        for (const p of merged.items) {
          const mk = keyOf(p);
          if (mk === null || mk === "<<" || direct.has(mk) || out.some((e) => e.key === mk))
            continue;
          out.push({
            key: mk,
            value: textOf(doc, p.value),
            lock: "anchor",
            direct: false,
            ref: null,
            anchored: false,
          });
        }
      }
      continue;
    }
    const v = pair.value;
    const value = textOf(doc, v);
    let lock: Lock | null = null;
    if (isAlias(v) || isCollection(v)) lock = "anchor";
    else if (VARIABLE_RE.test(value)) lock = "variable";
    out.push({ key: k, value, lock, direct: true, ref: pair, anchored: anchorOf(v) });
  }
  return out;
}

const isIngress = (e: Entry) => e.key.startsWith(PREFIX);

// splitKey reads the route name and the field out of an ingress key, as internal/ingress does. A
// key that has neither form, or whose route name the server refuses, is not a route the panel
// shows: the check reports it.
function splitKey(key: string): { name: string; field: RouteField } | null {
  const [first = "", second, ...rest] = key.slice(PREFIX.length).split(".");
  if (rest.length > 0) return null;
  if (second === undefined) {
    return isField(first) ? { name: DEFAULT_ROUTE, field: first } : null;
  }
  if (!isField(second) || isField(first) || first === DEFAULT_ROUTE || !NAME_RE.test(first))
    return null;
  return { name: first, field: second };
}

function routeKey(route: string, field: RouteField): string {
  return route === DEFAULT_ROUTE ? PREFIX + field : `${PREFIX}${route}.${field}`;
}

function routesOf(entries: Entry[]): ExposeRoute[] {
  const routes = new Map<string, ExposeRoute>();
  for (const e of entries) {
    if (!isIngress(e)) continue;
    const parts = splitKey(e.key);
    if (!parts) continue;
    let r = routes.get(parts.name);
    if (!r) {
      r = {
        name: parts.name,
        host: "",
        port: "",
        path: "",
        scheme: "http",
        strip: false,
        locked: {},
      };
      routes.set(parts.name, r);
    }
    const value = e.value.trim();
    switch (parts.field) {
      case "host":
        r.host = value;
        break;
      case "port":
        r.port = value;
        break;
      case "path":
        r.path = value;
        break;
      case "scheme":
        r.scheme = value.toLowerCase() === "https" ? "https" : "http";
        break;
      case "strip":
        r.strip = value.toLowerCase() === "true";
        break;
    }
    if (e.lock) r.locked[parts.field] = e.lock;
  }
  return [...routes.values()];
}

// lookup finds a key of a map the way compose reads it: a key of the map itself, else the first
// of its merge keys that has it, looking through aliases.
function lookup(doc: Document.Parsed, map: YAMLMap, key: string, depth = 0): unknown {
  if (depth > 8) return undefined;
  const pair = findPair(map, key);
  if (pair) return isAlias(pair.value) ? pair.value.resolve(doc) : pair.value;
  for (const p of map.items) {
    if (keyOf(p) !== "<<") continue;
    for (const merged of mergedMaps(doc, p.value)) {
      const v = lookup(doc, merged, key, depth + 1);
      if (v !== undefined) return v;
    }
  }
  return undefined;
}

// readEntries are the entries of a located block for reading. A block the editor cannot write, one
// that is an alias or comes in through a merge key, is still read, with every entry locked.
function readEntries(doc: Document.Parsed, svc: YAMLMap, where: Where, at: BlockRef): Entry[] {
  if (at.state === "found") return entriesOf(doc, at.block);
  if (at.state === "absent") return [];
  let holder: unknown = svc;
  if (where === "deploy") holder = isMap(holder) ? lookup(doc, holder, "deploy") : undefined;
  const labels = isMap(holder) ? lookup(doc, holder, "labels") : undefined;
  if (!isMap(labels) && !isSeq(labels)) return [];
  return entriesOf(doc, labels).map((e) => ({ ...e, lock: "anchor", direct: false, ref: null }));
}

// listServices are the names of the services of the file, in file order. A file that is not valid
// YAML has none.
export function listServices(text: string): string[] {
  const doc = load(text);
  const services = doc ? servicesOf(doc) : null;
  if (!services) return [];
  return services.items.map(keyOf).filter((k): k is string => k !== null);
}

// readServiceRoutes reads the routes of one service back from the block the target reads, and says
// whether labels sit in the other block (D-18).
export function readServiceRoutes(text: string, service: string, target: ExposeTarget): ExposeRead {
  const none: ExposeRead = {
    services: [],
    routes: [],
    form: "none",
    wrongPlace: false,
    bothPlaces: false,
    parseError: false,
    blockLocked: false,
  };
  const doc = load(text);
  if (!doc) return { ...none, parseError: true };
  const services = servicesOf(doc);
  const names = services ? services.items.map(keyOf).filter((k): k is string => k !== null) : [];
  const svc = serviceOf(doc, service);
  if (!svc) return { ...none, services: names };

  const right = locate(svc, whereOf(target));
  const wrong = locate(svc, otherWhere(whereOf(target)));
  const rightEntries = readEntries(doc, svc, whereOf(target), right);
  const wrongHas = readEntries(doc, svc, otherWhere(whereOf(target)), wrong).some(isIngress);
  const rightHas = rightEntries.some(isIngress);
  return {
    services: names,
    routes: routesOf(rightEntries),
    form: right.state === "found" ? (isMap(right.block) ? "map" : "list") : "none",
    wrongPlace: wrongHas && !rightHas,
    bothPlaces: wrongHas && rightHas,
    parseError: false,
    blockLocked: right.state === "locked",
  };
}

// ---- writing ----

// styleOf reads the indentation the file uses, so the whole document is written back the way it
// was: yaml indents a sequence under its key and uses two spaces unless told otherwise.
function styleOf(text: string): { indent: number; indentSeq: boolean } {
  const lines = text.split("\n");
  let indent = 2;
  for (const line of lines) {
    const m = /^( +)\S/.exec(line);
    if (m?.[1] && !line.trimStart().startsWith("#")) {
      indent = Math.min(Math.max(m[1].length, 1), 8);
      break;
    }
  }
  const flush = /^( *)[^\s#-][^\n]*:[ \t]*(#[^\n]*)?\n\1- /m.test(text);
  return { indent, indentSeq: !flush };
}

// ---- keeping the rest of the file as it was ----
//
// doc.toString writes the whole document, and it normalises what it did not need to touch: line
// endings become LF, 1e3 becomes 1e+3, a flow sequence gains inner spaces, a folded block is
// re-flowed. The editor must change nothing but the labels it was asked to change, so render writes
// the document twice, as parsed (base) and as edited (mod), and takes only the lines that differ
// between the two onto the original text. An original line that the writer would have normalised is
// left alone unless the edit is on that very line; then the whole edited text is the answer.

type Match = [number, number];

// matchLines pairs equal lines of a and b in order (a longest common subsequence). Both ends are
// trimmed first, since a label edit leaves almost everything common. It returns null when the
// differing middle is too large to compare.
function matchLines(a: string[], b: string[]): Match[] | null {
  let head = 0;
  while (head < a.length && head < b.length && a[head] === b[head]) head++;
  let tail = 0;
  while (
    tail < a.length - head &&
    tail < b.length - head &&
    a[a.length - 1 - tail] === b[b.length - 1 - tail]
  )
    tail++;
  const n = a.length - head - tail;
  const m = b.length - head - tail;
  if (n * m > 4_000_000) return null;
  const width = m + 1;
  const len = new Uint32Array((n + 1) * width);
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      len[i * width + j] =
        a[head + i] === b[head + j]
          ? (len[(i + 1) * width + j + 1] as number) + 1
          : Math.max(len[(i + 1) * width + j] as number, len[i * width + j + 1] as number);
    }
  }
  const out: Match[] = [];
  for (let i = 0; i < head; i++) out.push([i, i]);
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[head + i] === b[head + j]) {
      out.push([head + i, head + j]);
      i++;
      j++;
    } else if ((len[(i + 1) * width + j] as number) >= (len[i * width + j + 1] as number)) i++;
    else j++;
  }
  for (let k = 0; k < tail; k++) out.push([a.length - tail + k, b.length - tail + k]);
  return out;
}

function stripEol(line: string): string {
  return line.replace(/\r?\n$/, "");
}

// carryEdits applies to orig the lines that differ between base and mod, or returns null when an
// edit does not sit on lines that orig and base share.
function carryEdits(orig: string[], base: string[], mod: string[], eol: string): string[] | null {
  const origKeys = orig.map(stripEol);
  const baseOrig = matchLines(base, origKeys);
  const edits = matchLines(base, mod);
  if (!baseOrig || !edits) return null;
  const toOrig = new Array<number>(base.length).fill(-1);
  for (const [bi, oi] of baseOrig) toOrig[bi] = oi;

  const out: string[] = [];
  let cursor = 0;
  const push = (lines: string[]) => {
    for (const line of lines) {
      const last = out.length - 1;
      if (last >= 0 && !/\n$/.test(out[last] as string)) out[last] = (out[last] as string) + eol;
      out.push(line + eol);
    }
  };
  const copyTo = (to: number) => {
    for (; cursor < to; cursor++) out.push(orig[cursor] as string);
  };

  let pb = 0;
  let pm = 0;
  for (const [bi, mi] of [...edits, [base.length, mod.length] as Match]) {
    if (bi > pb || mi > pm) {
      const added = mod.slice(pm, mi);
      let from: number;
      let to: number;
      if (bi > pb) {
        from = toOrig[pb] as number;
        to = toOrig[bi - 1] as number;
        if (from < 0 || to < 0 || to - from !== bi - 1 - pb) return null;
        for (let k = pb; k < bi; k++) if (toOrig[k] !== from + (k - pb)) return null;
        to += 1;
      } else {
        from = pb === 0 ? 0 : (toOrig[pb - 1] as number) + 1;
        if (pb > 0 && from === 0) return null;
        if (pb < base.length && toOrig[pb] !== from) return null;
        to = from;
      }
      if (from < cursor) return null;
      copyTo(from);
      push(added);
      cursor = to;
    }
    pb = bi + 1;
    pm = mi + 1;
  }
  copyTo(orig.length);
  return out;
}

function render(doc: Document.Parsed, text: string): string {
  const opts = { lineWidth: 0, ...styleOf(text) };
  const mod = doc.toString(opts);
  const eol = /\r?\n/.exec(text)?.[0] === "\r\n" ? "\r\n" : "\n";
  const lines = (s: string) => s.split(/(?<=\n)/);
  const orig = lines(text);
  const modLines = lines(mod).map(stripEol);
  const baseLines = lines(parseDocument(text, PARSE).toString(opts)).map(stripEol);
  const merged = carryEdits(orig, baseLines, modLines, eol);
  return merged ? merged.join("") : eol === "\n" ? mod : mod.replace(/\n/g, eol);
}

function pruneEmpty(svc: YAMLMap, where: Where): void {
  let holder: YAMLMap = svc;
  if (where === "deploy") {
    const d = findPair(svc, "deploy");
    if (!d || !isMap(d.value)) return;
    holder = d.value;
  }
  const labels = findPair(holder, "labels");
  if (labels && isCollection(labels.value) && labels.value.items.length === 0) {
    holder.items.splice(holder.items.indexOf(labels), 1);
  }
  if (where === "deploy" && holder.items.length === 0) {
    const d = findPair(svc, "deploy");
    if (d) svc.items.splice(svc.items.indexOf(d), 1);
  }
}

function setChild(doc: Document.Parsed, map: YAMLMap, key: string, node: Node): void {
  const pair = findPair(map, key);
  if (pair) pair.value = node;
  else map.items.push(new Pair(doc.createNode(key), node));
}

// ensureBlock makes the labels block of a service that has none, in the given form, and returns it.
// The caller has seen that the block is absent and not locked.
function ensureBlock(
  doc: Document.Parsed,
  svc: YAMLMap,
  where: Where,
  form: "map" | "list",
): Block | null {
  let holder = svc;
  if (where === "deploy") {
    const d = findPair(svc, "deploy");
    if (d && isMap(d.value)) holder = d.value;
    else if (!d || isNullNode(d.value)) {
      const made = new YAMLMap(doc.schema);
      setChild(doc, svc, "deploy", made);
      holder = made;
    } else return null;
  }
  const block = form === "map" ? new YAMLMap(doc.schema) : new YAMLSeq(doc.schema);
  setChild(doc, holder, "labels", block);
  return block;
}

function appendEntry(doc: Document.Parsed, block: Block, key: string, value: string): void {
  if (isSeq(block)) block.items.push(doc.createNode(`${key}=${value}`));
  else block.items.push(doc.createPair(key, value) as Pair);
}

// writeEntry sets a key of a block: it changes the value of the entry that is there, or appends one.
// It returns false when nothing changed.
function writeEntry(
  doc: Document.Parsed,
  block: Block,
  entries: Entry[],
  key: string,
  value: string,
): boolean {
  const at = entries.find((e) => e.key === key);
  if (!at) {
    appendEntry(doc, block, key, value);
    return true;
  }
  if (at.lock || !at.direct || at.ref === null) return false;
  if (at.value === value) return false;
  if (isScalar(at.ref)) {
    at.ref.value = `${key}=${value}`;
  } else if (isScalar(at.ref.value)) {
    at.ref.value.value = value;
  } else {
    at.ref.value = doc.createNode(value);
  }
  return true;
}

// textOfValue is the text a field is written as. Surrounding space is never meant.
function valueText(value: string | boolean): string {
  return typeof value === "boolean" ? String(value) : value.trim();
}

// omitted says that a field at its default is left out of the file: a path of "" or "/", the http
// scheme, a strip that is off.
function omitted(field: RouteField, value: string): boolean {
  switch (field) {
    case "path":
      return value === "" || value === "/";
    case "scheme":
      return value === "" || value.toLowerCase() === "http";
    case "strip":
      return value === "" || value.toLowerCase() === "false";
    default:
      return false;
  }
}

function validRoute(route: string): boolean {
  return route === DEFAULT_ROUTE || NAME_RE.test(route);
}

// setRouteField sets one field of one route of a service, in the block the target reads. It returns
// the text unchanged when nothing should change: the file is not valid YAML, the service or the
// block is not there to edit, the field is locked, the value is the one already written, or a
// required field (host, port) is empty.
export function setRouteField(
  text: string,
  service: string,
  target: ExposeTarget,
  route: string,
  field: RouteField,
  value: string | boolean,
): string {
  const v = valueText(value);
  if (!validRoute(route) || !isField(field)) return text;
  if ((field === "host" || field === "port") && v === "") return text;
  const doc = load(text);
  const svc = doc ? serviceOf(doc, service) : null;
  if (!doc || !svc) return text;
  const where = whereOf(target);
  const at = locate(svc, where);
  if (at.state === "locked") return text;

  const key = routeKey(route, field);
  if (at.state === "absent") {
    if (omitted(field, v)) return text;
    const block = ensureBlock(doc, svc, where, "map");
    if (!block) return text;
    appendEntry(doc, block, key, v);
    return render(doc, text);
  }
  const entries = entriesOf(doc, at.block);
  const existing = entries.find((e) => e.key === key);
  if (omitted(field, v)) {
    if (
      !existing ||
      existing.lock ||
      !existing.direct ||
      existing.ref === null ||
      existing.anchored
    )
      return text;
    at.block.items.splice(at.block.items.indexOf(existing.ref as never), 1);
    pruneEmpty(svc, where);
    return render(doc, text);
  }
  if (!writeEntry(doc, at.block, entries, key, v)) return text;
  return render(doc, text);
}

// ---- adding and removing routes ----

// validateRouteName judges the name of a new named route: lower-case letters, digits and inner
// hyphens, 32 at most, never "default" (the short form's own name), and not one the service has.
export function validateRouteName(
  name: string,
  existing: readonly string[],
): "ok" | "invalid" | "duplicate" {
  if (name === DEFAULT_ROUTE || !NAME_RE.test(name)) return "invalid";
  return existing.includes(name) ? "duplicate" : "ok";
}

// validatePort says whether value is a whole number from 1 to 65535, digits only.
export function validatePort(value: string): boolean {
  if (!/^[0-9]+$/.test(value)) return false;
  const n = Number(value);
  return n >= 1 && n <= 65535;
}

// addRoute adds a route of a host and a port to a service. A null name is the short form, which a
// service has once; a name is a named route. It returns the text unchanged when the route would
// not be valid to add: a second short form, a name that is invalid or taken, an empty host or a
// port that is not 1 to 65535, or a service or block that cannot be edited.
export function addRoute(
  text: string,
  service: string,
  target: ExposeTarget,
  name: string | null,
  host: string,
  port: string,
): string {
  const h = host.trim();
  const p = port.trim();
  if (h === "" || !validatePort(p)) return text;
  const doc = load(text);
  const svc = doc ? serviceOf(doc, service) : null;
  if (!doc || !svc) return text;
  const where = whereOf(target);
  const at = locate(svc, where);
  if (at.state === "locked") return text;

  const route = name ?? DEFAULT_ROUTE;
  const taken = at.state === "found" ? routesOf(entriesOf(doc, at.block)).map((r) => r.name) : [];
  if (name === null ? taken.includes(DEFAULT_ROUTE) : validateRouteName(name, taken) !== "ok")
    return text;

  const block = at.state === "found" ? at.block : ensureBlock(doc, svc, where, "map");
  if (!block) return text;
  appendEntry(doc, block, routeKey(route, "host"), h);
  appendEntry(doc, block, routeKey(route, "port"), p);
  return render(doc, text);
}

// removeEntries deletes the given direct entries from a block.
function removeEntries(block: Block, entries: Entry[]): void {
  for (const e of entries) {
    const at = (block.items as unknown[]).indexOf(e.ref);
    if (at >= 0) block.items.splice(at, 1);
  }
}

// removeRoute deletes every key of one route from the block the target reads, in either form. When
// that was the service's last route, the ingress keys that mean nothing are deleted too, then the
// labels block if it is empty, then the deploy block if that is empty. A route written through an
// anchor is left alone: the key it was declared with may be used again elsewhere.
export function removeRoute(
  text: string,
  service: string,
  target: ExposeTarget,
  route: string,
): string {
  if (!validRoute(route)) return text;
  const doc = load(text);
  const svc = doc ? serviceOf(doc, service) : null;
  if (!doc || !svc) return text;
  const where = whereOf(target);
  const at = locate(svc, where);
  if (at.state !== "found") return text;

  const ofRoute = entriesOf(doc, at.block).filter(
    (e) => e.direct && isIngress(e) && splitKey(e.key)?.name === route,
  );
  if (ofRoute.length === 0 || ofRoute.some((e) => e.anchored)) return text;
  removeEntries(at.block, ofRoute);

  const left = entriesOf(doc, at.block);
  if (routesOf(left).length === 0) {
    removeEntries(
      at.block,
      left.filter((e) => e.direct && isIngress(e) && !e.anchored),
    );
  }
  pruneEmpty(svc, where);
  return render(doc, text);
}

// ---- labels in the wrong place, or in both ----

// ingressOf are the ingress entries of a block.
function ingressOf(doc: Document.Parsed, block: Block): Entry[] {
  return entriesOf(doc, block).filter(isIngress);
}

// moveToTarget moves the tierplan.ingress.* entries from the block the target does not read to the
// one it reads, keeping their form: a list's items stay items and a map's pairs stay pairs, as the
// very nodes, so quotes and comments go with them. When the target's block already exists in the
// other form the entries are written in that form. It does nothing unless the labels sit only in
// the wrong block, and it never moves a value set by an anchor or a merge key.
export function moveToTarget(text: string, service: string, target: ExposeTarget): string {
  const doc = load(text);
  const svc = doc ? serviceOf(doc, service) : null;
  if (!doc || !svc) return text;
  const where = whereOf(target);
  const from = locate(svc, otherWhere(where));
  const to = locate(svc, where);
  if (from.state !== "found" || to.state === "locked") return text;
  const moving = ingressOf(doc, from.block);
  if (moving.length === 0) return text;
  if (moving.some((e) => !e.direct || e.anchored || e.lock === "anchor")) return text;
  if (to.state === "found" && ingressOf(doc, to.block).length > 0) return text;

  const block =
    to.state === "found"
      ? to.block
      : ensureBlock(doc, svc, where, isMap(from.block) ? "map" : "list");
  if (!block) return text;
  for (const e of moving) {
    if (isMap(block) === isMap(from.block)) (block.items as unknown[]).push(e.ref);
    else appendEntry(doc, block, e.key, e.value);
  }
  removeEntries(from.block, moving);
  pruneEmpty(svc, otherWhere(where));
  return render(doc, text);
}

// removeIgnoredCopy deletes the tierplan.ingress.* entries from the block the target does not read,
// when the block it does read has them too: that copy is the one the server ignores. It never
// deletes the only copy, and it leaves the text alone when an entry comes from an anchor or a merge
// key.
export function removeIgnoredCopy(text: string, service: string, target: ExposeTarget): string {
  const doc = load(text);
  const svc = doc ? serviceOf(doc, service) : null;
  if (!doc || !svc) return text;
  const where = whereOf(target);
  const ignored = locate(svc, otherWhere(where));
  const used = locate(svc, where);
  if (ignored.state !== "found" || used.state !== "found") return text;
  if (ingressOf(doc, used.block).length === 0) return text;
  const going = ingressOf(doc, ignored.block);
  if (going.length === 0 || going.some((e) => !e.direct || e.anchored)) return text;
  removeEntries(ignored.block, going);
  pruneEmpty(svc, otherWhere(where));
  return render(doc, text);
}
