// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { AccessLogCard } from "@/features/ingress/access-log-card";
import { IngressCard } from "@/features/ingress/ingress-card";
import { IngressJoinTokenCard } from "@/features/ingress/join-token-card";
import { VIPCard } from "@/features/ingress/vip-card";

// SettingsTab is the Settings tab of the Ingress page (UI-SPEC S6): the switches of the environment, one card each,
// stacked. It works while ingress is off, since the switch that turns ingress on lives here (D-03, D-05).
export function SettingsTab({ envId }: { envId: string }) {
  return (
    <div className="flex flex-col gap-6">
      <IngressCard envId={envId} />
      <VIPCard envId={envId} />
      <AccessLogCard envId={envId} />
      <IngressJoinTokenCard envId={envId} />
    </div>
  );
}
