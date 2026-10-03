// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Compile-only stubs: the base path is not read yet.
export function basePath(): string {
  return "";
}

export function withBase(path: string): string {
  return path;
}

export function stripBase(path: string): string {
  return path;
}

export function apiUrl(path: string): string {
  return path;
}

export function resetBasePathForTests(): void {}
