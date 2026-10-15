// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// RED stub: replaced in the next commit.
export function getPath(_spec: unknown, _path: string): unknown {
  return undefined;
}

export function setPath<T>(spec: T, _path: string, _value: unknown): T {
  return spec;
}

export function removePath<T>(spec: T, _path: string): T {
  return spec;
}
