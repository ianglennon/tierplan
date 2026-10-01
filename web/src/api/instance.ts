// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { queryClient } from "@/api/query-client";

// The ["instance"] query: instance name, whether setup is still needed and the
// remember-me length. Route guards and the setup wizard share this key.
export const instanceQuery = queryOptions({
  queryKey: ["instance"],
  queryFn: async () => {
    const { data } = await api.GET("/api/instance");
    if (!data) throw new Error("GET /api/instance failed");
    return data;
  },
});

// loadInstance is for route loaders. staleTime 0 makes it fetch again each
// navigation, so the redirect between / and /setup never acts on an answer
// from before the admin was created.
export function loadInstance() {
  return queryClient.fetchQuery({ ...instanceQuery, staleTime: 0 });
}
