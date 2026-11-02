// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// The few Node APIs the helpers use. @types/node is not one of the packages
// approved for this project (01-01 Task 1), so they are declared here.
declare module "node:child_process" {
  export function execFileSync(
    file: string,
    args: readonly string[],
    options?: { encoding?: "utf8"; stdio?: unknown; timeout?: number; maxBuffer?: number },
  ): string;
}

declare module "node:fs" {
  export function readFileSync(path: string, encoding: "utf8"): string;
  export function mkdirSync(path: string, options?: { recursive?: boolean }): void;
}

declare module "node:path" {
  export function dirname(path: string): string;
  export function resolve(...parts: string[]): string;
}

declare module "node:url" {
  export function fileURLToPath(url: string): string;
}

declare const process: { env: Record<string, string | undefined> };

interface ImportMeta {
  url: string;
}
