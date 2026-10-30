// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

// safeNext turns the `next` search parameter into a path the browser may be
// sent to after sign-in. Only a same-origin path that starts with a single "/"
// is followed; anything else, including "//host", "/\host", a full URL and
// control characters, falls back to "/" (T-01-57).
export function safeNext(next: string | undefined): string {
  if (!next?.startsWith("/")) return "/";
  if (next.startsWith("//") || next.startsWith("/\\")) return "/";
  // Backslashes are treated as slashes by browsers, and control characters can
  // hide a scheme or a host from a check on the raw text.
  for (const ch of next) {
    const code = ch.charCodeAt(0);
    if (ch === "\\" || code < 0x20 || code === 0x7f) return "/";
  }
  return next;
}
