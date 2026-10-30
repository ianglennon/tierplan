// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// The placement constraint evaluator for the browser (RES-12). It follows swarmkit's
// manager/constraint package, which is what the scheduler runs, and is the twin of
// internal/envs/constraint.go: both read constraint-vectors.json, so they cannot
// disagree about which nodes a constraint matches. The operators are == and != only,
// values compare without regard to case, the names of labels do not, a field a node
// lacks is the empty string, and a key nobody defined matches no node.

export interface Constraint {
  key: string;
  value: string;
  // True for ==, false for !=.
  equal: boolean;
}

// The facts of one node that a constraint reads.
export interface NodeFacts {
  id: string;
  hostname: string;
  ip: string;
  role: string;
  os: string;
  arch: string;
  labels: Readonly<Record<string, string>>;
  engineLabels: Readonly<Record<string, string>>;
}

export type ParseResult = { ok: true; constraint: Constraint } | { ok: false; error: string };

export type MatchResult = { ok: true; matches: boolean } | { ok: false; error: string };

// swarmkit's patterns, anchored, with no nested quantifiers. RE2's \s is the ASCII
// white space only, so it is written out.
const KEY = /^[a-z_][a-z0-9\-_.]+$/i;
const VALUE = /^[a-z0-9:\-_ \t\n\f\r.*()?+[$^\]/\\]+$/i;

const OPERATORS = [
  { text: "==", equal: true },
  { text: "!=", equal: false },
] as const;

// parseConstraint reads one constraint of the form key==value or key!=value. The key and
// the value are trimmed. Another operator, an empty side or a character the swarm does
// not accept is an error.
export function parseConstraint(s: string): ParseResult {
  for (const op of OPERATORS) {
    const at = s.indexOf(op.text);
    if (at < 0) continue;
    const key = s.slice(0, at).trim();
    const value = s.slice(at + op.text.length).trim();
    if (!KEY.test(key)) return { ok: false, error: `constraint "${s}": key "${key}" is invalid` };
    if (!VALUE.test(value)) {
      return { ok: false, error: `constraint "${s}": value "${value}" is invalid` };
    }
    return { ok: true, constraint: { key, value, equal: op.equal } };
  }
  return { ok: false, error: `constraint "${s}": expected key==value or key!=value` };
}

// An address as net.ParseIP reads it: 128 bits, an IPv4 address held as ::ffff:a.b.c.d,
// and whether it is an IPv4 address (a dotted quad or its IPv4-mapped form).
interface Address {
  value: bigint;
  v4: boolean;
}

const DOTTED = /^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$/;
const HEX_GROUP = /^[0-9a-fA-F]{1,4}$/;
const MAPPED = 0xffffn << 32n;

function parseDotted(s: string): bigint | null {
  const m = DOTTED.exec(s);
  if (!m) return null;
  let out = 0n;
  for (let i = 1; i <= 4; i++) {
    const octet = m[i] as string;
    // Go refuses a leading zero, which some tools read as octal.
    if (octet.length > 1 && octet.startsWith("0")) return null;
    const n = Number(octet);
    if (n > 255) return null;
    out = (out << 8n) | BigInt(n);
  }
  return out;
}

function parseAddress(s: string): Address | null {
  const dotted = parseDotted(s);
  if (dotted !== null) return { value: MAPPED | dotted, v4: true };
  if (!s.includes(":")) return null;
  // An IPv4 address may end an IPv6 one: ::ffff:10.1.2.3.
  let text = s;
  const lastColon = s.lastIndexOf(":");
  const tail = s.slice(lastColon + 1);
  if (tail.includes(".")) {
    const v4 = parseDotted(tail);
    if (v4 === null) return null;
    text = `${s.slice(0, lastColon + 1)}${(v4 >> 16n).toString(16)}:${(v4 & 0xffffn).toString(16)}`;
  }
  const halves = text.split("::");
  if (halves.length > 2) return null;
  const groups = (part: string): string[] | null => {
    if (part === "") return [];
    const g = part.split(":");
    return g.every((x) => HEX_GROUP.test(x)) ? g : null;
  };
  const head = groups(halves[0] as string);
  const rest = halves.length === 2 ? groups(halves[1] as string) : [];
  if (head === null || rest === null) return null;
  let all: string[];
  if (halves.length === 2) {
    // "::" stands for at least one group of zeros.
    if (head.length + rest.length > 7) return null;
    all = [...head, ...Array<string>(8 - head.length - rest.length).fill("0"), ...rest];
  } else {
    if (head.length !== 8) return null;
    all = head;
  }
  let value = 0n;
  for (const g of all) value = (value << 16n) | BigInt(Number.parseInt(g, 16));
  // A mapped address is an IPv4 one, as in Go (To4).
  return { value, v4: value >> 32n === 0xffffn };
}

// inSubnet reads "address/length" as net.ParseCIDR does and says whether node is inside.
// A network of one family holds no address of the other.
function inSubnet(cidr: string, node: Address | null): boolean | null {
  const parts = cidr.split("/");
  if (parts.length !== 2) return null;
  const [addr, len] = parts as [string, string];
  if (!/^[0-9]+$/.test(len)) return null;
  const base = parseAddress(addr);
  if (base === null) return null;
  // The family of a network is the syntax of its address: the IPv4-mapped form written
  // in IPv6 syntax is an IPv6 network.
  const v4Syntax = parseDotted(addr) !== null;
  const bits = Number(len);
  if (bits > (v4Syntax ? 32 : 128)) return null;
  if (node === null || node.v4 !== v4Syntax) return false;
  const prefix = BigInt(v4Syntax ? 96 + bits : bits);
  const hostBits = 128n - prefix;
  return node.value >> hostBits === base.value >> hostBits;
}

function matchIP(c: Constraint, addr: string): boolean {
  const node = parseAddress(addr);
  const single = parseAddress(c.value);
  if (single !== null) return (node !== null && node.value === single.value) === c.equal;
  const within = inSubnet(c.value, node);
  // A value that is neither an address nor a network matches no node.
  if (within === null) return false;
  return within === c.equal;
}

function matchValue(c: Constraint, got: string): boolean {
  return (c.value.toLowerCase() === got.toLowerCase()) === c.equal;
}

function labelOf(labels: Readonly<Record<string, string>>, name: string): string {
  return Object.hasOwn(labels, name) ? (labels[name] as string) : "";
}

// matchesConstraint says whether the node satisfies one parsed constraint.
export function matchesConstraint(c: Constraint, n: NodeFacts): boolean {
  switch (c.key.toLowerCase()) {
    case "node.id":
      return matchValue(c, n.id);
    case "node.hostname":
      return matchValue(c, n.hostname);
    case "node.ip":
      return matchIP(c, n.ip);
    case "node.role":
      return matchValue(c, n.role);
    case "node.platform.os":
      return matchValue(c, n.os);
    case "node.platform.arch":
      return matchValue(c, n.arch);
  }
  // The prefixes are case-sensitive and so are the label names, as in swarmkit.
  if (c.key.startsWith("node.labels.")) {
    return matchValue(c, labelOf(n.labels, c.key.slice("node.labels.".length)));
  }
  if (c.key.startsWith("engine.labels.")) {
    return matchValue(c, labelOf(n.engineLabels, c.key.slice("engine.labels.".length)));
  }
  // A key the swarm does not define matches no node, whichever the operator.
  return false;
}

// matchesAll says whether the node satisfies every constraint; no constraints match every
// node. A constraint that does not parse is an error naming it, so a typo is never read
// as "no node matches".
export function matchesAll(constraints: readonly string[], n: NodeFacts): MatchResult {
  const parsed: Constraint[] = [];
  for (const s of constraints) {
    const r = parseConstraint(s);
    if (!r.ok) return r;
    parsed.push(r.constraint);
  }
  return { ok: true, matches: parsed.every((c) => matchesConstraint(c, n)) };
}
