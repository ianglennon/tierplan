// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

// Whether the desktop sidebar is expanded or collapsed to the icon rail,
// remembered per browser (D-53). The shadcn sidebar writes a cookie for this by
// default; TierPlan keeps it in localStorage so the server never sees it.

const STORAGE_KEY = "tierplan-sidebar";

export function getSidebarOpen(): boolean {
  try {
    return window.localStorage.getItem(STORAGE_KEY) !== "collapsed";
  } catch {
    return true;
  }
}

export function setSidebarOpen(open: boolean): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, open ? "expanded" : "collapsed");
  } catch {
    // The choice still applies for this page view.
  }
}
