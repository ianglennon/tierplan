// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The text the pool guidance gives a person to copy (D-23). Everything here is pure: nothing is sent
// anywhere and nothing is run. A value only reaches a command or a snippet as a canonical IPv4 CIDR
// or an integer that was checked first, so what is typed in a field can never add a second command
// to a line someone pastes into a shell (T-10-60). A value that fails produces an error, never a
// command: the builders throw, and the readers say which field was wrong.

import { type Cidr, parseCidr } from "@/features/networks/cidr";

// The ranges a swarm pool is cut from, lowest first (D-23 part 1).
const POOL_RANGES = ["10.0.0.0/8", "172.16.0.0/12"] as const;

// The sizes of one network the form offers: the swarm's --default-addr-pool-mask-length.
export const SWARM_MASKS = [24, 25, 26, 27, 28] as const;

export const DEFAULT_NETWORKS = 50;
export const DEFAULT_NODES = 5;
export const DEFAULT_MASK = 26;

// A swarm that needs more networks than this is not planned with this form.
export const MAX_NETWORKS = 100_000;
export const MAX_NODES = 10_000;

// A daemon.json pool's size runs up to this (Docker refuses a smaller block).
const MAX_POOL_SIZE = 30;

// A stack name Docker accepts and a shell reads as one word.
const STACK_NAME = /^[A-Za-z0-9_-]+$/;

const WIDTH = 32;

function formatV4(n: bigint): string {
  return [24n, 16n, 8n, 0n].map((shift) => String((n >> shift) & 255n)).join(".");
}

// canonical writes an IPv4 prefix the one way: four octets, the network address, a length. It is
// null for anything else, IPv6 included.
export function canonicalCidr(input: string): string | null {
  const cidr = parseCidr(input);
  if (cidr === null || cidr.family !== 4) return null;
  return `${formatV4(cidr.base)}/${cidr.bits}`;
}

function isInt(n: unknown): n is number {
  return typeof n === "number" && Number.isInteger(n);
}

// readInt reads digits only, so "5e2", "0x10" and " 7;" are not numbers.
export function readInt(text: string): number | null {
  const s = text.trim();
  if (!/^\d{1,9}$/.test(s)) return null;
  return Number(s);
}

export interface AvoidRead {
  ranges: Cidr[];
  // The 1-based lines that are not an IPv4 CIDR. Blank lines are left out.
  badLines: number[];
}

// parseAvoidRanges reads the Ranges to avoid box: one IPv4 CIDR a line. A line that is anything
// else, such as "10.0.0.0/8; rm -rf /", is reported by number and read no further.
export function parseAvoidRanges(text: string): AvoidRead {
  const ranges: Cidr[] = [];
  const badLines: number[] = [];
  text.split(/\r?\n/).forEach((line, i) => {
    if (line.trim() === "") return;
    const cidr = parseCidr(line);
    if (cidr === null || cidr.family !== 4) badLines.push(i + 1);
    else ranges.push(cidr);
  });
  return { ranges, badLines };
}

export interface SwarmPlanInput {
  networks: number;
  nodes: number;
  mask: number;
  avoid: readonly Cidr[];
}

export interface SwarmPlan {
  // The pool to give --default-addr-pool.
  pool: string;
  mask: number;
  // How many networks of the mask the pool holds.
  capacity: number;
  // capacity less the networks expected.
  spare: number;
  networks: number;
  // The addresses an overlay of the mask can hand out.
  usable: number;
  // True when usable is fewer than the nodes expected, each of which takes an address on every
  // overlay for its load-balancer endpoint.
  tooSmallForNodes: boolean;
  nodes: number;
}

export type SwarmPlanResult = { plan: SwarmPlan } | { error: "invalid" | "no_fit" };

// usableOf is what a subnet of the mask holds: its size less the network, broadcast and gateway
// addresses.
function usableOf(mask: number): number {
  return 2 ** (WIDTH - mask) - 3;
}

function nextPowerOfTwo(n: number): { power: number; value: number } {
  let power = 0;
  while (2 ** power < n) power++;
  return { power, value: 2 ** power };
}

function lastAddress(base: bigint, bits: number): bigint {
  return base + (1n << BigInt(WIDTH - bits)) - 1n;
}

// firstFree is the lowest block of the length inside the range that overlaps none of the avoided
// ones, or null. Blocks are aligned to their own size, so the walk steps by that size and jumps
// past an avoided range as soon as it meets one.
function firstFree(range: Cidr, bits: number, avoid: readonly Cidr[]): bigint | null {
  if (bits < range.bits) return null;
  const size = 1n << BigInt(WIDTH - bits);
  const end = lastAddress(range.base, range.bits);
  let start = range.base;
  while (start + size - 1n <= end) {
    const blockEnd = start + size - 1n;
    let hit: bigint | null = null;
    for (const a of avoid) {
      const aEnd = lastAddress(a.base, a.bits);
      if (a.base <= blockEnd && aEnd >= start && (hit === null || aEnd > hit)) hit = aEnd;
    }
    if (hit === null) return start;
    // The next aligned block that starts after the avoided range ends.
    start = ((hit + size) / size) * size;
  }
  return null;
}

// planSwarmPool is the pool a new swarm should be created with: the lowest aligned block inside
// 10.0.0.0/8, then 172.16.0.0/12, that holds the next power of two at or above 1.5 times the
// networks expected at the chosen size and overlaps none of the ranges to avoid. The 1.5 leaves
// room for a rolling update, which briefly holds a network twice.
export function planSwarmPool(input: SwarmPlanInput): SwarmPlanResult {
  const { networks, nodes, mask } = input;
  if (
    !isInt(networks) ||
    networks < 1 ||
    networks > MAX_NETWORKS ||
    !isInt(nodes) ||
    nodes < 1 ||
    nodes > MAX_NODES ||
    !SWARM_MASKS.some((m) => m === mask)
  ) {
    return { error: "invalid" };
  }
  const { power, value } = nextPowerOfTwo(Math.ceil(networks * 1.5));
  const bits = mask - power;
  for (const text of POOL_RANGES) {
    const range = parseCidr(text);
    if (range === null) continue;
    const found = firstFree(range, bits, input.avoid);
    if (found === null) continue;
    const usable = usableOf(mask);
    return {
      plan: {
        pool: `${formatV4(found)}/${bits}`,
        mask,
        capacity: value,
        spare: value - networks,
        networks,
        usable,
        tooSmallForNodes: usable < nodes,
        nodes,
      },
    };
  }
  return { error: "no_fit" };
}

// swarmInitCommand builds the swarm init line from a canonical IPv4 CIDR and an integer length, and
// throws on anything else. --advertise-addr is left for the person to add.
export function swarmInitCommand(pool: string, mask: number): string {
  const canonical = canonicalCidr(pool);
  if (canonical === null || canonical !== pool) throw new Error("pool is not a canonical CIDR");
  if (!isInt(mask) || mask < 8 || mask > MAX_POOL_SIZE) throw new Error("mask is not a length");
  return `docker swarm init --default-addr-pool ${pool} --default-addr-pool-mask-length ${mask}`;
}

export interface SwarmInputs {
  // The fields as typed, so a half-typed value is still a value.
  networks: string;
  nodes: string;
  mask: string;
  avoid: string;
}

export const DEFAULT_SWARM_INPUTS: SwarmInputs = {
  networks: String(DEFAULT_NETWORKS),
  nodes: String(DEFAULT_NODES),
  mask: String(DEFAULT_MASK),
  avoid: "",
};

export type SwarmRead =
  | { kind: "ok"; plan: SwarmPlan; command: string }
  | { kind: "no_fit" }
  | {
      kind: "invalid";
      networks: boolean;
      nodes: boolean;
      // The 1-based lines of the Ranges to avoid box that are not a CIDR.
      avoidLines: number[];
    };

// readSwarmInputs turns the typed fields into the plan and the command, or says which field is
// wrong. A field that fails gives its error and no command.
export function readSwarmInputs(raw: SwarmInputs): SwarmRead {
  const networks = readInt(raw.networks);
  const nodes = readInt(raw.nodes);
  const mask = readInt(raw.mask);
  const avoid = parseAvoidRanges(raw.avoid);
  const badNetworks = networks === null || networks < 1 || networks > MAX_NETWORKS;
  const badNodes = nodes === null || nodes < 1 || nodes > MAX_NODES;
  if (badNetworks || badNodes || mask === null || avoid.badLines.length > 0) {
    return { kind: "invalid", networks: badNetworks, nodes: badNodes, avoidLines: avoid.badLines };
  }
  const result = planSwarmPool({ networks, nodes, mask, avoid: avoid.ranges });
  if ("error" in result) {
    return result.error === "no_fit"
      ? { kind: "no_fit" }
      : { kind: "invalid", networks: false, nodes: false, avoidLines: [] };
  }
  return { kind: "ok", plan: result.plan, command: swarmInitCommand(result.plan.pool, mask) };
}

export interface PoolRowInput {
  base: string;
  size: string;
}

// daemonJsonSnippet is the default-address-pools setting for daemon.json: valid JSON, two spaces
// deep, one object a row. A row whose base is not a canonical IPv4 CIDR, or whose size is not a
// whole number from the base's own length to 30, makes it throw.
export function daemonJsonSnippet(rows: readonly PoolRowInput[]): string {
  const pools = rows.map((row) => {
    const base = canonicalCidr(row.base);
    const cidr = base === null ? null : parseCidr(base);
    const size = readInt(row.size);
    // A base with host bits is a mistake to fix, not one to correct silently.
    if (base === null || base !== row.base.trim() || cidr === null || size === null) {
      throw new Error("pool row is not valid");
    }
    if (size < cidr.bits || size > MAX_POOL_SIZE) throw new Error("pool size is out of range");
    return { base, size };
  });
  if (pools.length === 0) throw new Error("no pools");
  return JSON.stringify({ "default-address-pools": pools }, null, 2);
}

// stackRemoveCommands is a docker stack rm line for each name that is one plain word, in the order
// given. A name with a space, a quote, a ';' or anything else a shell reads is left out.
export function stackRemoveCommands(names: readonly string[]): string[] {
  return names.filter((n) => STACK_NAME.test(n)).map((n) => `docker stack rm ${n}`);
}

export const SWARM_LEAVE_COMMAND = "docker swarm leave --force";
export const JOIN_TOKEN_COMMAND = "docker swarm join-token worker";
