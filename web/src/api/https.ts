// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { components } from "@/api/schema";

export type HTTPSStatus = components["schemas"]["HTTPSStatusBody"];
export type HTTPSCertificate = components["schemas"]["HTTPSCertificateBody"];

// The ["https", "status"] query: the certificate the listener serves now, whatever its source.
// It is read again every minute and when the window gets focus back, so a renewal shows up.
export const httpsStatusQuery = queryOptions({
  queryKey: ["https", "status"],
  queryFn: async (): Promise<HTTPSStatus> => {
    const { data } = await api.GET("/api/https/status");
    if (!data) throw new Error("GET /api/https/status failed");
    return data;
  },
  refetchInterval: 60_000,
  refetchOnWindowFocus: true,
});
