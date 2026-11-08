// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { readFileSync } from "node:fs";
import https from "node:https";
import { Socket } from "node:net";
import tls from "node:tls";
import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Development only. make dev passes DEV_PUBLIC_URL here so the host name a remote
// browser uses gets past Vite's host check, which otherwise answers only
// localhost and IP addresses (G-02-1b). Unset or unparsable leaves Vite's default.
function devAllowedHosts(): string[] | undefined {
  const raw = process.env.TIERPLAN_DEV_PUBLIC_URL;
  if (!raw) return undefined;
  try {
    return [new URL(raw).hostname];
  } catch {
    return undefined;
  }
}

// Development only (plan 13.1-05, D-08). make dev serves the SPA over https with a development
// certificate (TIERPLAN_DEV_TLS_DIR holds cert.pem and key.pem; vitest and vite build set no such
// variable and need no files).
function devHttps(): { key: Buffer; cert: Buffer } | undefined {
  const dir = process.env.TIERPLAN_DEV_TLS_DIR;
  if (!dir) return undefined;
  return { key: readFileSync(`${dir}/key.pem`), cert: readFileSync(`${dir}/cert.pem`) };
}

// The Go server's own certificate, which it writes into its data directory
// (<DataDir>/tls/server.crt). It is read again for every new upstream connection, so a Go server
// that starts after Vite, or one that signed a new certificate, is trusted without restarting
// Vite. It is the only trust anchor the proxy has: Node accepts a self-signed certificate that is
// listed as its own anchor, and still checks the name and the dates.
const upstreamCertFile =
  process.env.TIERPLAN_DEV_SERVER_CERT ??
  decodeURIComponent(new URL("../.devdata/tls/server.crt", import.meta.url).pathname);

class VerifiedUpstreamAgent extends https.Agent {
  // biome-ignore lint/suspicious/noExplicitAny: node's Agent types leave the options open.
  createConnection(options: any, callback?: (err: Error | null, socket: any) => void) {
    let ca: Buffer;
    try {
      ca = readFileSync(upstreamCertFile);
    } catch (err) {
      // No certificate to trust yet: fail this connection, never fall back to trusting anything.
      const socket = new Socket();
      process.nextTick(() => socket.destroy(err as Error));
      return socket;
    }
    return tls.connect({ ...options, ca }, callback);
  }
}

const upstream = process.env.TIERPLAN_DEV_API_TARGET ?? "https://127.0.0.1:7480";
const upstreamAgent = new VerifiedUpstreamAgent();

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
    __TIERPLAN_SOURCE_URL__: JSON.stringify(
      process.env.TIERPLAN_SOURCE_URL ?? "https://github.com/ianglennon/tierplan",
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
    https: devHttps(),
    // Dev loop: the SPA runs on Vite, the API on the Go server (make dev), both over https.
    // The proxy verifies the Go server's certificate against the server's own copy of it
    // (VerifiedUpstreamAgent above); there is no setting that turns that check off.
    // changeOrigin stays false so the browser's Origin header
    // (https://localhost:5173) reaches the Go Origin check unchanged (Pitfall 20).
    // ws forwards the browser shell's WebSocket upgrade; without it the shell's
    // pending session is never claimed and expires after 30 seconds.
    // /hooks: under make dev the webhook address TierPlan shows is built on the Vite public
    // URL, so a git host's delivery to /hooks/git/{token} must reach the Go server. It is a
    // plain POST, so no ws.
    proxy: {
      "/api": {
        target: upstream,
        changeOrigin: false,
        ws: true,
        secure: true,
        agent: upstreamAgent,
      },
      "/hooks": { target: upstream, changeOrigin: false, secure: true, agent: upstreamAgent },
    },
  },
});
