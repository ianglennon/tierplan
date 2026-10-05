// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// Bayplan can be served under a path prefix behind a reverse proxy (D-50). The
// server announces the prefix in <meta name="bayplan-base"> when it renders
// index.html (plan 02-03); every URL the SPA builds goes through the helpers
// here, so nothing else needs to know about it. Without the tag (the Vite dev
// server, tests) the base is empty and the SPA runs at the root.

let cached: string | null = null;

// basePath is the announced prefix without a trailing slash, or "" for none.
// It is read once: the page cannot move to another base without a reload.
export function basePath(): string {
  if (cached !== null) return cached;
  const raw =
    document.querySelector<HTMLMetaElement>('meta[name="bayplan-base"]')?.content.trim() ?? "";
  cached = raw.endsWith("/") ? raw.slice(0, -1) : raw;
  return cached;
}

// Forgets the remembered base so a test can announce another one.
export function resetBasePathForTests(): void {
  cached = null;
}

// withBase turns an app path ("/env/a") into the path the browser uses.
export function withBase(path: string): string {
  return basePath() + path;
}

// stripBase is the reverse of withBase for a path taken from the address bar
// or a request URL. A path that only shares the prefix ("/bayplanx") is not
// under the base and comes back unchanged.
export function stripBase(path: string): string {
  const base = basePath();
  if (base === "") return path;
  if (path === base) return "/";
  if (path.startsWith(`${base}/`)) return path.slice(base.length);
  return path;
}

// apiUrl is withBase named for API, stream and download URLs.
export function apiUrl(path: string): string {
  return withBase(path);
}
