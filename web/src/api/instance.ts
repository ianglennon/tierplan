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
  // Pages that read the instance (the document title, the sign-in heading) do
  // not refetch it on every mount. A mutation that changes the name still
  // invalidates ["instance"], which refetches regardless of staleTime.
  staleTime: 30_000,
});

// loadInstance is for route loaders. The guards that decide between /setup and
// the app (login, setup, welcome) call it with the default staleTime of 0, so
// they fetch fresh and never act on an answer from before the admin was
// created. The signed-in guard passes 30_000 and reuses an answer up to 30 s
// old, so a click between signed-in pages does not hinge on a network round
// trip (WR-18). The staleTime given here overrides the query's own.
export function loadInstance(staleTime = 0) {
  return queryClient.fetchQuery({ ...instanceQuery, staleTime });
}
