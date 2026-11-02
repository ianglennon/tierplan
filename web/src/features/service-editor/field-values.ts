// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// What the Form tab's text fields stand for in a spec: whole numbers, port numbers, cores as
// NanoCPUs and memory as bytes. A function answers null for text that is not such a value, and
// the field then says so and writes nothing.

export const MIB = 1024 ** 2;
export const GIB = 1024 ** 3;

// wholeNumber is a number of 0 or more written as digits only.
export function wholeNumber(text: string): number | null {
  const t = text.trim();
  if (!/^\d+$/.test(t)) return null;
  const n = Number(t);
  return Number.isSafeInteger(n) ? n : null;
}

// portNumber is a port from 1 to 65535.
export function portNumber(text: string): number | null {
  const n = wholeNumber(text);
  return n !== null && n >= 1 && n <= 65535 ? n : null;
}

// coresToNano is cores like 0.5 as the NanoCPUs Docker stores (a billionth of a core).
export function coresToNano(text: string): number | null {
  const t = text.trim();
  if (!/^\d+(\.\d+)?$/.test(t)) return null;
  const nano = Math.round(Number(t) * 1e9);
  return Number.isSafeInteger(nano) ? nano : null;
}

// nanoToCores is NanoCPUs as cores, without trailing zeros, or "" for no limit.
export function nanoToCores(nano: unknown): string {
  if (typeof nano !== "number" || !Number.isFinite(nano) || nano <= 0) return "";
  return (nano / 1e9).toFixed(9).replace(/\.?0+$/, "");
}

export type MemoryUnit = "MiB" | "GiB";

// memoryToBytes is a whole number of MiB or GiB as bytes.
export function memoryToBytes(text: string, unit: MemoryUnit): number | null {
  const n = wholeNumber(text);
  if (n === null) return null;
  const bytes = n * (unit === "GiB" ? GIB : MIB);
  return Number.isSafeInteger(bytes) ? bytes : null;
}

// bytesToMemory is a size in bytes as the number and unit the field shows: GiB when the size is
// a whole number of them, MiB otherwise. A size that is not a whole number of MiB shows as MiB
// with decimals, which is only a display: nothing is written until the person edits the field.
export function bytesToMemory(bytes: unknown): { text: string; unit: MemoryUnit } {
  if (typeof bytes !== "number" || !Number.isFinite(bytes) || bytes <= 0) {
    return { text: "", unit: "MiB" };
  }
  if (bytes % GIB === 0) return { text: String(bytes / GIB), unit: "GiB" };
  if (bytes % MIB === 0) return { text: String(bytes / MIB), unit: "MiB" };
  return { text: String(Number((bytes / MIB).toFixed(3))), unit: "MiB" };
}

// A service name as the Form accepts it on create.
export const NAME_PATTERN = /^[A-Za-z0-9._-]{1,63}$/;
