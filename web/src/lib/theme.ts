// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Theme choice, remembered per browser in localStorage (D-63). This is the same
// logic as public/theme-init.js, which applies the saved choice before first
// paint; this module changes it afterwards and keeps "system" in step with the
// operating system.

export type ThemeMode = "light" | "dark" | "system";

const STORAGE_KEY = "tierplan-theme";

function isMode(v: unknown): v is ThemeMode {
  return v === "light" || v === "dark" || v === "system";
}

export function getTheme(): ThemeMode {
  try {
    const saved = window.localStorage.getItem(STORAGE_KEY);
    if (isMode(saved)) return saved;
  } catch {
    // localStorage can be blocked; follow the system.
  }
  return "system";
}

function prefersDark(): boolean {
  return window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function apply(mode: ThemeMode): void {
  const dark = mode === "dark" || (mode === "system" && prefersDark());
  document.documentElement.classList.toggle("dark", dark);
}

export function setTheme(mode: ThemeMode): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, mode);
  } catch {
    // The choice still applies for this page view.
  }
  apply(mode);
}

// watchSystemTheme re-applies the theme when the operating system changes, but
// only while the choice is "system". It returns a function that stops watching.
export function watchSystemTheme(): () => void {
  const query = window.matchMedia("(prefers-color-scheme: dark)");
  const onChange = () => {
    if (getTheme() === "system") apply("system");
  };
  query.addEventListener("change", onChange);
  return () => query.removeEventListener("change", onChange);
}
