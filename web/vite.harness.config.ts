// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

// Serves the test-only copy harness (web/harness/copy) to the Playwright suite
// in e2e/copy. It is not part of vite.config.ts's build and never reaches the
// binary.
export default defineConfig({
  root: "harness/copy",
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": decodeURIComponent(new URL("./src", import.meta.url).pathname),
    },
  },
  server: {
    // Vite otherwise answers only localhost and IP addresses. The suite opens
    // the page as http://tierplan.test, a LAN-style name (an insecure context).
    allowedHosts: ["tierplan.test"],
  },
});
