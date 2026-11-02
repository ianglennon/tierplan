// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { HealthState } from "@/api/environments";

// What the Nodes list and the node page both say about a node, in one place: its state, its
// platform and its CPUs.

// The states the swarm gives a node, as the UI-SPEC colours them: ready is online, down and
// disconnected are offline, anything else (unknown, or a word this version does not know) is
// neutral and reads Unknown.
export type NodeStateKey = "ready" | "down" | "disconnected" | "unknown";

export function nodeStateKey(state: string): NodeStateKey {
  return state === "ready" || state === "down" || state === "disconnected" ? state : "unknown";
}

const TONES: Record<NodeStateKey, HealthState> = {
  ready: "online",
  down: "offline",
  disconnected: "offline",
  unknown: "unknown",
};

export function nodeTone(state: string): HealthState {
  return TONES[nodeStateKey(state)];
}

// A node whose state is down or disconnected: the two the swarm cannot count on.
export function isNodeDown(state: string): boolean {
  return state === "down" || state === "disconnected";
}

// The engine names an architecture the Linux way (x86_64, aarch64); Docker's own platform names
// are amd64, arm64 and so on. A name this does not know is shown as it came.
const ARCH: Record<string, string> = {
  x86_64: "amd64",
  amd64: "amd64",
  aarch64: "arm64",
  arm64: "arm64",
  armv7l: "arm",
  armv6l: "arm",
  armhf: "arm",
  i386: "386",
  i686: "386",
  ppc64le: "ppc64le",
  s390x: "s390x",
  riscv64: "riscv64",
};

export function archName(arch: string): string {
  return ARCH[arch] ?? arch;
}

// platformOf is "linux/arm64": the operating system and the architecture. A node that reports
// only one of them shows that one; one that reports neither is an empty string, which callers
// show as a dash.
export function platformOf(os: string, arch: string): string {
  const a = arch ? archName(arch) : "";
  if (os && a) return `${os}/${a}`;
  return os || a;
}

// formatCpus is a node's CPU count: a whole number as it is, a fraction to one place. A node that
// reports none is an empty string.
export function formatCpus(cpus: number): string {
  if (!Number.isFinite(cpus) || cpus <= 0) return "";
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 }).format(cpus);
}
