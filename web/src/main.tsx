// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createRouter, RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

// Before anything that builds a schema: see the file for why.
import "@/lib/zod-config";

import "@fontsource-variable/geist";
import "@fontsource-variable/geist-mono";
import "./index.css";
import "@/i18n";
import { queryClient } from "@/api/query-client";
import { basePath } from "@/lib/base-path";
import { watchSystemTheme } from "@/lib/theme";
import { routeTree } from "./routeTree.gen";

const router = createRouter({ routeTree, basepath: basePath() || undefined });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

// Keeps "System" in step with the operating system for as long as the page lives.
watchSystemTheme();

const root = document.getElementById("root");
if (!root) throw new Error("missing #root element");

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
