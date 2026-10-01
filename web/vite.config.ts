// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

export default defineConfig({
  plugins: [react()],
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
    // Dev loop: the SPA runs on Vite, the API on the Go server.
    proxy: { "/api": "http://127.0.0.1:7480" },
  },
});
