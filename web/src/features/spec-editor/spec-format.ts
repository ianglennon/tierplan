// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import {
  Document,
  isAlias,
  isMap,
  isScalar,
  isSeq,
  LineCounter,
  type Node,
  parseDocument,
  visit,
} from "yaml";

import i18n from "@/i18n";

// The spec as text, in the two formats the Spec tab offers (D-01). A spec is converted between
// them through its parsed value and never through its text, so what comes back is what went in:
// numbers, booleans, nulls, empty strings, key order and nested lists survive, and anything the
// editor cannot hold exactly (an integer JavaScript would round) is refused instead of changed.

export type SpecFormat = "yaml" | "json";

export type SpecValue = Record<string, unknown>;

export type ParseResult =
  | { ok: true; value: SpecValue }
  | { ok: false; line: number; message: string };

// The one thing kept in browser storage about the editor (UI-SPEC "Storage").
export const SPEC_FORMAT_KEY = "coaming.spec-format";

type Params = Record<string, unknown>;

function say(key: string, params?: Params): string {
  return String(i18n.t(`specEditor.parse.${key}`, { ns: "specEditor", ...params }));
}

// ---- storage ----

// loadSpecFormat is the format a person chose last, YAML until they choose. Anything else that
// is stored, and storage that is off, is YAML.
export function loadSpecFormat(): SpecFormat {
  try {
    return localStorage.getItem(SPEC_FORMAT_KEY) === "json" ? "json" : "yaml";
  } catch {
    return "yaml";
  }
}

export function saveSpecFormat(format: SpecFormat): void {
  try {
    localStorage.setItem(SPEC_FORMAT_KEY, format);
  } catch {
    // Storage is full or off; the choice then lasts as long as the page.
  }
}

// ---- rendering ----

// The words an older YAML reads as booleans or null, quoted so any reader takes them as text.
const OLD_YAML_WORD = /^(y|n|yes|no|on|off|true|false|null|~)$/i;
// Text that looks like a number or a time in some YAML version: digits with separators, a sign,
// an exponent, a base prefix, or a date.
const NUMBER_LIKE = /^[-+]?[.\d][\d_.:,eExXoObBa-fA-F+-]*$/;
const DATE_LIKE = /^\d{4}-\d{2}-\d{2}/;

function ambiguous(text: string): boolean {
  return OLD_YAML_WORD.test(text) || NUMBER_LIKE.test(text) || DATE_LIKE.test(text);
}

// renderSpec writes a parsed spec as text. YAML keeps the order of the keys, never folds a long
// line, and quotes every string that could be read as something else.
export function renderSpec(value: unknown, format: SpecFormat): string {
  if (format === "json") return JSON.stringify(value, null, 2);
  const doc = new Document(value, { aliasDuplicateObjects: false });
  visit(doc, {
    Scalar(_key, node) {
      if (typeof node.value === "string" && ambiguous(node.value) && !node.value.includes("\n")) {
        node.type = "QUOTE_DOUBLE";
      }
    },
  });
  return doc.toString({ lineWidth: 0, indent: 2 });
}

// ---- parsing ----

function lineAt(text: string, offset: number): number {
  let line = 1;
  const end = Math.min(offset, text.length);
  for (let i = 0; i < end; i++) if (text.charCodeAt(i) === 10) line++;
  return line;
}

function fail(line: number, message: string): ParseResult {
  return { ok: false, line, message };
}

// A refusal found while walking the text, with the line it is on.
class SpecError extends Error {
  readonly line: number;
  constructor(line: number, message: string) {
    super(message);
    this.line = line;
  }
}

// Every value the editor holds must be one JSON writes exactly. The numbers are checked here: an
// integer above 2^53 would be rounded by JavaScript and so changed in silence.
function checkNumber(text: string, value: number | bigint, path: string, line: number): number {
  if (typeof value === "bigint") {
    if (value > BigInt(Number.MAX_SAFE_INTEGER) || value < -BigInt(Number.MAX_SAFE_INTEGER)) {
      throw new SpecError(line, say("unsafeInteger", { path, value: text }));
    }
    return Number(value);
  }
  if (!Number.isFinite(value)) throw new SpecError(line, say("notFinite", { path }));
  return value;
}

// setKey adds a property as an own property, so a key named __proto__ is data and never the
// object's prototype.
function setKey(target: SpecValue, key: string, value: unknown): void {
  Object.defineProperty(target, key, {
    value,
    enumerable: true,
    writable: true,
    configurable: true,
  });
}

function childPath(path: string, key: string): string {
  return path === "" ? key : `${path}.${key}`;
}

// ---- JSON ----

const JSON_SPACE = new Set([" ", "\t", "\n", "\r"]);
const JSON_NUMBER = /-?(?:0|[1-9]\d*)(\.\d+)?([eE][+-]?\d+)?/y;
const JSON_ESCAPES: Record<string, string> = {
  '"': '"',
  "\\": "\\",
  "/": "/",
  b: "\b",
  f: "\f",
  n: "\n",
  r: "\r",
  t: "\t",
};
// Deeper than any service spec, and shallow enough for the call stack.
const MAX_DEPTH = 200;

// JsonReader reads JSON text by hand, so every refusal has the place it is at in every browser
// (the message of JSON.parse names none in some) and an integer is looked at before it is rounded.
class JsonReader {
  private i = 0;

  constructor(private readonly text: string) {}

  read(): SpecValue {
    this.skip();
    const start = this.i;
    const value = this.value("", 0);
    this.skip();
    if (this.i < this.text.length) this.refuse(say("afterEnd"));
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      throw new SpecError(lineAt(this.text, start), say("notObject"));
    }
    return value as SpecValue;
  }

  private refuse(message: string, at = this.i): never {
    throw new SpecError(lineAt(this.text, at), message);
  }

  private unexpected(): never {
    if (this.i >= this.text.length) this.refuse(say("unexpectedEnd"));
    this.refuse(say("unexpectedChar", { char: JSON.stringify(this.text[this.i]) }));
  }

  private skip(): void {
    while (this.i < this.text.length && JSON_SPACE.has(this.text.charAt(this.i))) this.i++;
  }

  private value(path: string, depth: number): unknown {
    if (depth > MAX_DEPTH) this.refuse(say("tooDeep"));
    const c = this.text.charAt(this.i);
    if (c === "{") return this.object(path, depth);
    if (c === "[") return this.array(path, depth);
    if (c === '"') return this.string();
    if (c === "-" || (c >= "0" && c <= "9")) return this.number(path);
    for (const [word, value] of [
      ["true", true],
      ["false", false],
      ["null", null],
    ] as const) {
      if (this.text.startsWith(word, this.i)) {
        this.i += word.length;
        return value;
      }
    }
    return this.unexpected();
  }

  private object(path: string, depth: number): SpecValue {
    const out: SpecValue = {};
    this.i++;
    this.skip();
    if (this.text.charAt(this.i) === "}") {
      this.i++;
      return out;
    }
    for (;;) {
      this.skip();
      if (this.text.charAt(this.i) !== '"') {
        if (this.i >= this.text.length) this.unexpected();
        this.refuse(say("expectedKey"));
      }
      const keyAt = this.i;
      const key = this.string();
      if (Object.hasOwn(out, key)) this.refuse(say("duplicateKey", { key }), keyAt);
      this.skip();
      if (this.text.charAt(this.i) !== ":") {
        if (this.i >= this.text.length) this.unexpected();
        this.refuse(say("expectedColon"));
      }
      this.i++;
      this.skip();
      setKey(out, key, this.value(childPath(path, key), depth + 1));
      this.skip();
      const c = this.text.charAt(this.i);
      this.i++;
      if (c === "}") return out;
      if (c !== ",") {
        this.i--;
        if (this.i >= this.text.length) this.unexpected();
        this.refuse(say("expectedCommaOrEnd"));
      }
    }
  }

  private array(path: string, depth: number): unknown[] {
    const out: unknown[] = [];
    this.i++;
    this.skip();
    if (this.text.charAt(this.i) === "]") {
      this.i++;
      return out;
    }
    for (;;) {
      this.skip();
      out.push(this.value(`${path}[${out.length}]`, depth + 1));
      this.skip();
      const c = this.text.charAt(this.i);
      this.i++;
      if (c === "]") return out;
      if (c !== ",") {
        this.i--;
        if (this.i >= this.text.length) this.unexpected();
        this.refuse(say("expectedCommaOrEnd"));
      }
    }
  }

  private string(): string {
    const open = this.i;
    this.i++;
    let out = "";
    for (;;) {
      if (this.i >= this.text.length) this.refuse(say("unterminated"), open);
      const c = this.text.charAt(this.i);
      if (c === '"') {
        this.i++;
        return out;
      }
      if (c < " ") this.refuse(say("controlChar"));
      if (c !== "\\") {
        out += c;
        this.i++;
        continue;
      }
      const e = this.text.charAt(this.i + 1);
      const plain = JSON_ESCAPES[e];
      if (plain !== undefined) {
        out += plain;
        this.i += 2;
      } else if (e === "u" && /^[0-9a-fA-F]{4}$/.test(this.text.slice(this.i + 2, this.i + 6))) {
        out += String.fromCharCode(Number.parseInt(this.text.slice(this.i + 2, this.i + 6), 16));
        this.i += 6;
      } else {
        this.refuse(say("badEscape"));
      }
    }
  }

  private number(path: string): number {
    JSON_NUMBER.lastIndex = this.i;
    const m = JSON_NUMBER.exec(this.text);
    if (!m) this.refuse(say("badNumber"));
    const token = m[0];
    const line = lineAt(this.text, this.i);
    this.i += token.length;
    const whole = m[1] === undefined && m[2] === undefined;
    if (whole) {
      const n = Number(token);
      if (!Number.isSafeInteger(n))
        throw new SpecError(line, say("unsafeInteger", { path, value: token }));
      return n;
    }
    return checkNumber(token, Number(token), path, line);
  }
}

function parseJson(text: string): ParseResult {
  try {
    return { ok: true, value: new JsonReader(text).read() };
  } catch (e) {
    if (e instanceof SpecError) return fail(e.line, e.message);
    throw e;
  }
}

// ---- YAML ----

// The most nodes a document may expand to once its aliases are followed.
const MAX_NODES = 500_000;

// What the library says without the place it says it at, which the caller states itself.
const yamlMessage = (message: string) => message.replace(/ at line \d+, column \d+:[\s\S]*$/, "");

// walk turns a YAML node into the value it holds, looking at each number on the way.
function walk(
  doc: Document,
  node: unknown,
  path: string,
  lines: LineCounter,
  budget: { left: number },
): unknown {
  if (--budget.left < 0) throw new SpecError(1, say("tooBig"));
  const lineOf = (n: Node | null | undefined): number => {
    const at = n?.range?.[0];
    return at === undefined ? 1 : lines.linePos(at).line;
  };
  if (node === null || node === undefined) return null;
  if (isAlias(node)) {
    const target = node.resolve(doc);
    return target ? walk(doc, target, path, lines, budget) : null;
  }
  if (isScalar(node)) {
    const v = node.value;
    if (typeof v === "number" || typeof v === "bigint") {
      return checkNumber(String(v), v, path, lineOf(node));
    }
    if (v === null || typeof v === "string" || typeof v === "boolean") return v;
    return String(v);
  }
  if (isSeq(node)) {
    return node.items.map((item, i) => walk(doc, item, `${path}[${i}]`, lines, budget));
  }
  if (isMap(node)) {
    const out: SpecValue = {};
    for (const pair of node.items) {
      const k = pair.key;
      if (isAlias(k) || !isScalar(k)) throw new SpecError(lineOf(k as Node), say("complexKey"));
      const key = String(k.value);
      setKey(out, key, walk(doc, pair.value, childPath(path, key), lines, budget));
    }
    return out;
  }
  return null;
}

function parseYaml(text: string): ParseResult {
  const lines = new LineCounter();
  let doc: Document;
  try {
    doc = parseDocument(text, { lineCounter: lines, intAsBigInt: true, uniqueKeys: true });
  } catch {
    return fail(1, say("unexpectedEnd"));
  }
  const first = doc.errors[0];
  if (first) {
    const at = first.linePos?.[0]?.line;
    return fail(at ?? 1, yamlMessage(first.message));
  }
  if (doc.contents === null) return fail(1, say("empty"));
  try {
    const value = walk(doc, doc.contents, "", lines, { left: MAX_NODES });
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      const at = doc.contents.range?.[0];
      return fail(at === undefined ? 1 : lines.linePos(at).line, say("notObject"));
    }
    return { ok: true, value: value as SpecValue };
  } catch (e) {
    if (e instanceof SpecError) return fail(e.line, e.message);
    throw e;
  }
}

// parseSpec reads text in the given format. It never throws: a spec that does not parse, is not
// an object, or holds a number that cannot be held exactly comes back as the line it is on and
// what is wrong, in the words a person can act on.
export function parseSpec(text: string, format: SpecFormat): ParseResult {
  return format === "json" ? parseJson(text) : parseYaml(text);
}

// hasYamlComments says whether YAML text holds a comment, which would be lost in a switch to
// JSON. A hash inside a string is not one, so the text is read, not searched.
export function hasYamlComments(text: string): boolean {
  let doc: Document;
  try {
    doc = parseDocument(text);
  } catch {
    return false;
  }
  if (doc.commentBefore || doc.comment) return true;
  let found = false;
  visit(doc, {
    Node(_key, node) {
      if (node.comment || node.commentBefore) {
        found = true;
        return visit.BREAK;
      }
      return undefined;
    },
  });
  return found;
}
