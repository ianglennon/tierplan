// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// CIDR arithmetic for the overlap warning of the create dialog, and nothing else.
// The server parses with net/netip and has the last word on whether an address is
// valid; this only has to tell whether two prefixes share an address, so the form
// can say so before anything is sent. Addresses are BigInts, 32 or 128 bits wide.

export interface Cidr {
  family: 4 | 6;
  // The first address of the prefix: the host bits are dropped.
  base: bigint;
  bits: number;
}

const V4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const HEX_GROUP = /^[0-9a-fA-F]{1,4}$/;
// A prefix length is digits with no leading zero, as netip reads it.
const PREFIX_LEN = /^(0|[1-9]\d{0,2})$/;

function parseV4(s: string): bigint | null {
  const m = V4.exec(s);
  if (!m) return null;
  let out = 0n;
  for (let i = 1; i <= 4; i++) {
    const octet = m[i] as string;
    // netip refuses a leading zero, which some tools read as octal.
    if (octet.length > 1 && octet.startsWith("0")) return null;
    const n = Number(octet);
    if (n > 255) return null;
    out = (out << 8n) | BigInt(n);
  }
  return out;
}

function parseV6(s: string): bigint | null {
  if (!s.includes(":") || s.includes(":::")) return null;
  const halves = s.split("::");
  if (halves.length > 2) return null;
  const groups = (part: string): string[] | null => {
    if (part === "") return [];
    const g = part.split(":");
    return g.every((x) => HEX_GROUP.test(x)) ? g : null;
  };
  const head = groups(halves[0] as string);
  const tail = halves.length === 2 ? groups(halves[1] as string) : [];
  if (head === null || tail === null) return null;
  let all: string[];
  if (halves.length === 2) {
    // "::" stands for at least one group of zeros.
    if (head.length + tail.length > 7) return null;
    all = [...head, ...Array<string>(8 - head.length - tail.length).fill("0"), ...tail];
  } else {
    if (head.length !== 8) return null;
    all = head;
  }
  let out = 0n;
  for (const g of all) out = (out << 16n) | BigInt(Number.parseInt(g, 16));
  return out;
}

// parseCidr reads an IPv4 or IPv6 prefix, or null when it is not one. Host bits
// are dropped, as the server does. Surrounding spaces are ignored, because the
// form trims what is typed.
export function parseCidr(input: string): Cidr | null {
  const parts = input.trim().split("/");
  if (parts.length !== 2) return null;
  const [addr, len] = parts as [string, string];
  if (!PREFIX_LEN.test(len)) return null;
  const bits = Number(len);
  const v4 = parseV4(addr);
  const v6 = v4 === null ? parseV6(addr) : null;
  const family: 4 | 6 = v4 !== null ? 4 : 6;
  const raw = v4 ?? v6;
  if (raw === null) return null;
  const width = family === 4 ? 32 : 128;
  if (bits > width) return null;
  const hostBits = BigInt(width - bits);
  return { family, base: (raw >> hostBits) << hostBits, bits };
}

// formatV4 writes an IPv4 address as four dotted octets.
function formatV4(n: bigint): string {
  return [24n, 16n, 8n, 0n].map((shift) => String((n >> shift) & 255n)).join(".");
}

// splitV4 reads an IPv4 prefix without dropping its host bits: the address as typed, and the length.
function splitV4(input: string): { raw: bigint; bits: number } | null {
  const parts = input.trim().split("/");
  if (parts.length !== 2) return null;
  const [addr, len] = parts as [string, string];
  if (!PREFIX_LEN.test(len)) return null;
  const raw = parseV4(addr);
  const bits = Number(len);
  if (raw === null || bits > 32) return null;
  return { raw, bits };
}

// hostBitsOf says what an IPv4 subnet was meant to be when it has host bits set, for example
// 10.0.0.0/24 for 10.0.0.5/24. It is null for a subnet that is a network address, that is IPv6 or
// that is not a prefix at all. The server decides again; this is the instant feedback before it
// answers.
export function hostBitsOf(input: string): string | null {
  const v4 = splitV4(input);
  if (v4 === null) return null;
  const hostBits = BigInt(32 - v4.bits);
  const network = (v4.raw >> hostBits) << hostBits;
  return network === v4.raw ? null : `${formatV4(network)}/${v4.bits}`;
}

// tooSmall says whether an IPv4 subnet is smaller than a /29, which cannot hold a gateway and any
// containers.
export function tooSmall(input: string): boolean {
  const v4 = splitV4(input);
  return v4 !== null && v4.bits > 29;
}

// overlaps says whether two prefixes share an address: one contains the other, or
// they are equal. Adjacent prefixes do not overlap, prefixes of different
// families never do, and anything that is not a prefix overlaps nothing.
export function overlaps(a: string, b: string): boolean {
  const x = parseCidr(a);
  const y = parseCidr(b);
  if (x === null || y === null || x.family !== y.family) return false;
  const width = x.family === 4 ? 32 : 128;
  // The shorter prefix is the wider network: the two overlap when their first
  // address agrees on its bits.
  const shared = BigInt(width - Math.min(x.bits, y.bits));
  return x.base >> shared === y.base >> shared;
}

// parseAddress reads one IPv4 or IPv6 address, or null when it is not one.
function parseAddress(input: string): { family: 4 | 6; value: bigint } | null {
  const text = input.trim();
  const v4 = parseV4(text);
  if (v4 !== null) return { family: 4, value: v4 };
  const v6 = parseV6(text);
  return v6 === null ? null : { family: 6, value: v6 };
}

// containsAddress says whether the address is inside the prefix. An address of
// the other family, or a malformed one, is not.
export function containsAddress(cidr: string, address: string): boolean {
  const net = parseCidr(cidr);
  const addr = parseAddress(address);
  if (net === null || addr === null || net.family !== addr.family) return false;
  const hostBits = BigInt((net.family === 4 ? 32 : 128) - net.bits);
  return addr.value >> hostBits === net.base >> hostBits;
}

// containsCidr says whether the inner prefix lies wholly inside the outer one:
// no wider, and starting inside it.
export function containsCidr(outer: string, inner: string): boolean {
  const o = parseCidr(outer);
  const i = parseCidr(inner);
  if (o === null || i === null || o.family !== i.family || i.bits < o.bits) return false;
  const hostBits = BigInt((o.family === 4 ? 32 : 128) - o.bits);
  return i.base >> hostBits === o.base >> hostBits;
}

export interface Overlap {
  name: string;
  cidr: string;
}

export interface OverlapReport {
  // How many networks overlap, a network listed on several nodes counting once.
  total: number;
  // The first two, in the order the list has them.
  shown: Overlap[];
  // total less the ones shown.
  more: number;
}

interface HasSubnets {
  name: string;
  subnets?: readonly string[] | null;
}

const SHOWN = 2;

// overlapsWith finds the listed networks whose subnets overlap cidr. It is null
// when none does, and when cidr is not a prefix. A network is named once per
// overlapping subnet; the same name and subnet on several nodes (the default
// bridge) is one.
export function overlapsWith(cidr: string, networks: readonly HasSubnets[]): OverlapReport | null {
  if (parseCidr(cidr) === null) return null;
  const seen = new Set<string>();
  const found: Overlap[] = [];
  for (const n of networks) {
    for (const s of n.subnets ?? []) {
      if (!overlaps(cidr, s)) continue;
      const key = `${n.name}\u0000${s}`;
      if (seen.has(key)) continue;
      seen.add(key);
      found.push({ name: n.name, cidr: s });
    }
  }
  if (found.length === 0) return null;
  return {
    total: found.length,
    shown: found.slice(0, SHOWN),
    more: Math.max(0, found.length - SHOWN),
  };
}
