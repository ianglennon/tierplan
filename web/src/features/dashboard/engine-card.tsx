// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { EnvironmentSummary } from "@/api/environments";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";

interface EngineCardProps {
  // Absent while loading or when the summary could not be read: every value
  // then shows "—".
  engine?: EnvironmentSummary["engine"];
  loading?: boolean;
  // Agent environments only: how many agents are connected out of those the
  // swarm should have (a swarm only), and the agent's version.
  agents?: { connected: number; expected: number } | null;
  agentVersion?: string | null;
  isAgentEnvironment?: boolean;
}

const GIB = 1024 ** 3;

// EngineCard is the "Docker engine" card (D-55): key-value rows for what the
// daemon reports. Anything it does not report is "—", and values wrap rather
// than truncate.
export function EngineCard({ engine, loading }: EngineCardProps) {
  const t = useT();
  const missing = t("dashboard.engine.missing");

  const osArch = [engine?.os, engine?.arch].filter(Boolean).join(" / ");
  const memory =
    engine && engine.memTotal > 0
      ? t("dashboard.engine.memoryValue", {
          value: new Intl.NumberFormat(undefined, {
            minimumFractionDigits: 1,
            maximumFractionDigits: 1,
          }).format(engine.memTotal / GIB),
        })
      : undefined;
  const cpus = engine && engine.ncpu > 0 ? new Intl.NumberFormat().format(engine.ncpu) : undefined;

  const rows: ReadonlyArray<[string, string | undefined]> = [
    [t("dashboard.engine.version"), engine?.version],
    [t("dashboard.engine.apiVersion"), engine?.apiVersion],
    [t("dashboard.engine.osArch"), osArch || undefined],
    [t("dashboard.engine.storageDriver"), engine?.storageDriver],
    [t("dashboard.engine.cpus"), cpus],
    [t("dashboard.engine.memory"), memory],
  ];

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-base font-semibold">{t("dashboard.engine.title")}</CardTitle>
      </CardHeader>
      <CardContent>
        <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm">
          {rows.map(([label, value]) => (
            <div key={label} className="contents">
              <dt className="text-muted-foreground">{label}</dt>
              <dd className="min-w-0 break-words tabular-nums">
                {loading ? (
                  <Skeleton className="h-4 w-24" aria-hidden="true" />
                ) : (
                  (value ?? missing)
                )}
              </dd>
            </div>
          ))}
        </dl>
      </CardContent>
    </Card>
  );
}
