// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { stripBase } from "@/lib/base-path";

// The pages every environment has: its dashboard ("") and the four read-only
// lists. Switching keeps the user on one of these when the target has it, and
// every environment has all of them (a down one shows its own full-page state).
export const ENV_SECTIONS = ["", "containers", "images", "volumes", "networks"] as const;

// switchTarget is where choosing environment envId goes from pathname (D-35):
// the same section of that environment when the user is in one, else its
// dashboard. Anything below a section (a container's page) is left behind,
// because it belongs to the environment being left. The result is an app path,
// without the base prefix and ready for the router.
export function switchTarget(pathname: string, envId: string): string {
  const parts = stripBase(pathname).split("/").filter(Boolean);
  const base = `/env/${encodeURIComponent(envId)}`;
  if (parts[0] !== "env" || parts.length < 2) return base;
  const section = parts[2] ?? "";
  return section !== "" && ENV_SECTIONS.some((s) => s === section) ? `${base}/${section}` : base;
}

// isMacPlatform is true for Apple user agents, where the shortcut is the
// command key and not Control.
export function isMacPlatform(): boolean {
  return /Mac|iPhone|iPad|iPod/.test(navigator.userAgent);
}

// shortcutLabel is the palette shortcut as shown in a Kbd.
export function shortcutLabel(): string {
  return isMacPlatform() ? "⌘K" : "Ctrl K";
}
