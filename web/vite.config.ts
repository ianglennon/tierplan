// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Development only. make dev passes DEV_PUBLIC_URL here so the host name a remote
// browser uses gets past Vite's host check, which otherwise answers only
// localhost and IP addresses (G-02-1b). Unset or unparsable leaves Vite's default.
function devAllowedHosts(): string[] | undefined {
  const raw = process.env.BAYPLAN_DEV_PUBLIC_URL;
  if (!raw) return undefined;
  try {
    return [new URL(raw).hostname];
  } catch {
    return undefined;
  }
}

export default defineConfig({
  // Relative asset URLs: the built index.html and chunk loading carry no
  // absolute /assets/ or /theme-init.js. The Go server rewrites every "./"
  // src/href to the public URL's base path when it serves the page (plan 02-03,
  // D-50), so one build serves any subpath. The dev server treats "./" as "/".
  base: "./",
  plugins: [
    // The router plugin must come before the React plugin (RESEARCH Pitfall 21).
    tanstackRouter({
      target: "react",
      routesDirectory: "./src/routes",
      generatedRouteTree: "./src/routeTree.gen.ts",
      autoCodeSplitting: true,
    }),
    react(),
    tailwindcss(),
  ],
  define: {
    // Source link shown by VersionFooter when /api/version is unreachable (PLAT-05).
    __BAYPLAN_SOURCE_URL__: JSON.stringify(
      process.env.BAYPLAN_SOURCE_URL ?? "https://github.com/ianglennon/bayplan",
    ),
  },
  resolve: {
    alias: {
      "@": decodeURIComponent(new URL("./src", import.meta.url).pathname),
    },
  },
  build: {
    outDir: "dist",
    // Never inline assets as data: URIs; the CSP does not allow them (D-37).
    assetsInlineLimit: 0,
  },
  server: {
    allowedHosts: devAllowedHosts(),
    // Dev loop: the SPA runs on Vite, the API on the Go server (make dev).
    // changeOrigin stays false so the browser's Origin header
    // (http://localhost:5173) reaches the Go Origin check unchanged (Pitfall 20).
    // ws forwards the browser shell's WebSocket upgrade; without it the shell's
    // pending session is never claimed and expires after 30 seconds.
    // /hooks: under make dev the webhook address Bayplan shows is built on the Vite public
    // URL, so a git host's delivery to /hooks/git/{token} must reach the Go server. It is a
    // plain POST, so no ws.
    proxy: {
      "/api": { target: "http://127.0.0.1:7480", changeOrigin: false, ws: true },
      "/hooks": { target: "http://127.0.0.1:7480", changeOrigin: false },
    },
  },
});
