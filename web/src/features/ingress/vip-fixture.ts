// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { IngressVIP } from "@/api/ingress-nodes";

// A swarm's virtual IP as the server answers it: node-b holds the address, node-a stands by, nothing is wrong.
export function vipFixture(over: Partial<IngressVIP> = {}): IngressVIP {
  return {
    address: "192.168.10.250/24",
    routerId: 51,
    routerDerived: true,
    keyGeneration: 3,
    enabled: true,
    holder: { state: "one", nodes: ["sw-b"] },
    candidates: [
      {
        nodeId: "sw-a",
        hostname: "node-a",
        vrrpState: "BACKUP",
        proxyReady: true,
        taskState: "running",
        lastFault: "",
        lastHeard: "2026-11-20T10:05:00Z",
      },
      {
        nodeId: "sw-b",
        hostname: "node-b",
        vrrpState: "MASTER",
        proxyReady: true,
        taskState: "running",
        lastFault: "",
        lastHeard: "2026-11-20T10:05:00Z",
      },
    ],
    managers: [{ nodeId: "sw-a", hostname: "node-a", state: "allows", localSocket: false }],
    managersReadable: true,
    foreignRouter: { detected: false },
    clockSkewNodes: [],
    authWindowSeconds: 30,
    lastEnsure: { at: "2026-11-20T10:00:00Z" },
    ...over,
  };
}
