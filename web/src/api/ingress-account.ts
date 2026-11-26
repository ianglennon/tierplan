// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

// The install-wide state of the per-hostname (ingress) certificates: whether an Admin accepted the
// terms of the directories, and whether issuing is paused, as it is after a restore.
export type IngressAccount = components["schemas"]["IngressCertAccountBody"];
export type IngressAccountDirectory = components["schemas"]["IngressCertAccountDirectoryBody"];

// The ["ingress", "account"] query. It is a global settings.read route, so a caller enables it only for
// an instance admin. A refused or failed read is not retried by itself: the card offers a retry button.
export const ingressAccountQuery = queryOptions({
  queryKey: ["ingress", "account"],
  queryFn: async (): Promise<IngressAccount> => {
    const { data } = await api.GET("/api/ingress/certificates/account");
    if (!data) throw new Error("GET /api/ingress/certificates/account failed");
    return data;
  },
  retry: false,
});
