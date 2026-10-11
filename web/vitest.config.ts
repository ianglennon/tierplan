// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    alias: {
      "@": decodeURIComponent(new URL("./src", import.meta.url).pathname),
    },
  },
  define: {
    // Same build-time constant vite.config.ts defines, fixed for tests.
    __BAYPLAN_SOURCE_URL__: JSON.stringify("https://github.com/ianglennon/bayplan"),
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    // The CI runners are shared with the Docker-in-Docker jobs, and a loaded run takes
    // three or four times as long as a quiet one (vitest 831 s against 240 s), so the
    // default 5 s per test failed tests at random. A slow test still fails, just later.
    testTimeout: 20_000,
    // Stylesheets are empty text in tests unless named here. The log palette test reads
    // index.css as text to keep it in step with the colours in code.
    css: { include: [/src\/index\.css\?raw$/] },
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
});
