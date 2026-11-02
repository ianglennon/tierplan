// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Applies the saved theme before first paint. Served from the TierPlan origin as
// a plain blocking script, so the CSP can keep script-src 'self' with no inline
// script (D-37, D-63). Values: "light", "dark" or "system" (default).
(() => {
  let choice = "system";
  try {
    const saved = window.localStorage.getItem("tierplan-theme");
    if (saved === "light" || saved === "dark" || saved === "system") choice = saved;
  } catch {
    // localStorage can be blocked; fall back to following the system.
  }
  const dark =
    choice === "dark" ||
    (choice === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
})();
