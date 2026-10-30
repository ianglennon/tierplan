// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    alias: {
      "@": decodeURIComponent(new URL("./src", import.meta.url).pathname),
    },
  },
  define: {
    // Same build-time constant vite.config.ts defines, fixed for tests.
    __COAMING_SOURCE_URL__: JSON.stringify("https://github.com/ianglennon/coaming"),
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    // A deadline only decides how long a broken test takes to fail (docs/testing.md), so a long
    // one costs nothing when the test passes and a short one is a flake on a loaded runner. The
    // CI runners are shared with the Docker-in-Docker jobs, and a loaded run takes three or four
    // times as long as a quiet one (vitest 831 s against 240 s). A test or hook that is really
    // stuck still fails, just later.
    testTimeout: 60_000,
    hookTimeout: 60_000,
    // Stylesheets are empty text in tests unless named here. The log palette test reads
    // index.css as text to keep it in step with the colours in code.
    css: { include: [/src\/index\.css\?raw$/] },
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
  },
});
