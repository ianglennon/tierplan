// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import tailwindcss from "@tailwindcss/vite";
import { tanstackRouter } from "@tanstack/router-plugin/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
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
    // Dev loop: the SPA runs on Vite, the API on the Go server (make dev).
    // changeOrigin stays false so the browser's Origin header
    // (http://localhost:5173) reaches the Go Origin check unchanged (Pitfall 20).
    proxy: {
      "/api": { target: "http://127.0.0.1:7480", changeOrigin: false },
    },
  },
});
