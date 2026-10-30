// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient } from "@tanstack/react-query";

// One client for the app, importable by route loaders (beforeLoad runs outside
// React) as well as by the provider in main.tsx.
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false },
  },
});
