// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { type ServiceDetail, serviceQuery } from "@/api/services";
import { CopyableCode } from "@/components/copyable-code";
import { type KeyValueRow, KeyValueTable } from "@/components/key-value-table";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { canScale, itemOf } from "@/features/service-detail/header-actions";
import { ScaleDialog } from "@/features/services/scale-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

interface ServiceOverviewProps {
  envId: string;
  serviceId: string;
}

const key = "servicedetail.overview";

// What a row shows for a value that is not set.
function useMissing() {
  const t = useT();
  return t("servicedetail.missing");
}

// humanDuration writes a Go duration as a person would: 2m0s is 2m and 1h0m0s is 1h. An empty
// duration is not set.
export function humanDuration(d: string): string {
  return d.replace(/(\d+h)0m0s$/, "$1").replace(/(\d+[hm])0s$/, "$1");
}

// A table inside a card: it scrolls sideways when the card is narrow, and its text wraps at
// any character, in mono. Everything in it is text from Docker.
function CardTable<T>({
  label,
  columns,
  rows,
  rowKey,
  empty,
}: {
  label: string;
  columns: readonly { header: string; cell: (row: T) => ReactNode }[];
  rows: readonly T[];
  rowKey: (row: T, index: number) => string;
  empty: string;
}) {
  if (rows.length === 0) return <p className="text-sm text-muted-foreground">{empty}</p>;
  return (
    <Table aria-label={label}>
      <TableHeader>
        <TableRow>
          {columns.map((c) => (
            <TableHead key={c.header}>{c.header}</TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {rows.map((row, i) => (
          <TableRow key={rowKey(row, i)}>
            {columns.map((c) => (
              <TableCell
                key={c.header}
                className="align-top font-mono text-xs break-all whitespace-normal"
              >
                {c.cell(row)}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

// ScaledToZero is what a replicated service with no replicas says, with the way to give it
// some. It is information, not an error: scaling to 0 is a normal thing to do.
function ScaledToZero({ envId, detail }: { envId: string; detail: ServiceDetail }) {
  const t = useT();
  const needs = useNeeds("services.control", envId);
  const [open, setOpen] = useState(false);
  return (
    <div className="flex flex-wrap items-center gap-3 rounded-lg border bg-card px-4 py-3 text-sm">
      <span>{t(`${key}.scaledToZero.text`)}</span>
      <NeedsTooltip reason={needs.reason}>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="max-md:h-11"
          onClick={needs.allowed ? () => setOpen(true) : undefined}
        >
          {t(`${key}.scaledToZero.action`)}
        </Button>
      </NeedsTooltip>
      {needs.allowed ? (
        <ScaleDialog envId={envId} targets={[itemOf(detail)]} open={open} onOpenChange={setOpen} />
      ) : null}
    </div>
  );
}

function ServiceCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  const isJob = detail.mode === "replicated-job" || detail.mode === "global-job";
  return (
    <SettingsCard title={t(`${key}.service.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.service.title`)}
        empty=""
        rows={[
          {
            key: t(`${key}.service.id`),
            value: <CopyableCode code={detail.id} label={t(`${key}.service.idLabel`)} />,
          },
          { key: t(`${key}.service.mode`), value: t(`services.list.mode.${detail.mode}`) },
          {
            key: t(`${key}.service.replicas`),
            value: isJob
              ? missing
              : t(`${key}.service.replicasValue`, {
                  running: detail.running,
                  count: detail.desired,
                }),
          },
          { key: t(`${key}.service.created`), value: <RelativeTime value={detail.createdAt} /> },
          { key: t(`${key}.service.updated`), value: <RelativeTime value={detail.updatedAt} /> },
          { key: t(`${key}.service.stack`), value: detail.stack || missing },
        ]}
      />
    </SettingsCard>
  );
}

function ImageCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  const { image } = detail;
  return (
    <SettingsCard title={t(`${key}.image.title`)}>
      <KeyValueTable
        label={t(`${key}.image.title`)}
        empty=""
        rows={[
          { key: t(`${key}.image.image`), value: image.tag || image.ref || missing },
          {
            key: t(`${key}.image.digest`),
            value: image.digest ? (
              <CopyableCode code={image.digest} label={t(`${key}.image.digestLabel`)} />
            ) : (
              missing
            ),
          },
          {
            key: t(`${key}.image.pinned`),
            value: t(image.pinned ? `${key}.image.pinnedYes` : `${key}.image.pinnedNo`),
          },
        ]}
      />
    </SettingsCard>
  );
}

function PortsCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  return (
    <SettingsCard title={t(`${key}.ports.title`)}>
      <CardTable
        label={t(`${key}.ports.title`)}
        empty={t(`${key}.ports.empty`)}
        rows={detail.ports ?? []}
        rowKey={(p, i) => `${i}|${p.published}|${p.target}|${p.protocol}`}
        columns={[
          {
            header: t(`${key}.ports.published`),
            cell: (p) => (p.published > 0 ? String(p.published) : missing),
          },
          { header: t(`${key}.ports.target`), cell: (p) => String(p.target) },
          { header: t(`${key}.ports.protocol`), cell: (p) => p.protocol },
          {
            header: t(`${key}.ports.mode`),
            cell: (p) =>
              p.mode === "ingress"
                ? t(`${key}.ports.ingress`)
                : p.mode === "host"
                  ? t(`${key}.ports.host`)
                  : p.mode || missing,
          },
        ]}
      />
    </SettingsCard>
  );
}

function MountsCard({ envId, detail }: { envId: string; detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  return (
    <SettingsCard title={t(`${key}.mounts.title`)}>
      <CardTable
        label={t(`${key}.mounts.title`)}
        empty={t(`${key}.mounts.empty`)}
        rows={detail.mounts ?? []}
        rowKey={(m, i) => `${i}|${m.target}`}
        columns={[
          { header: t(`${key}.mounts.type`), cell: (m) => m.type },
          {
            header: t(`${key}.mounts.source`),
            cell: (m) =>
              m.type === "volume" && m.source ? (
                <Link
                  to="/env/$envId/volumes/$volumeName"
                  params={{ envId, volumeName: m.source }}
                  className="rounded-sm text-accent-ink hover:underline"
                >
                  {m.source}
                </Link>
              ) : (
                m.source || missing
              ),
          },
          { header: t(`${key}.mounts.destination`), cell: (m) => m.target },
          {
            header: t(`${key}.mounts.mode`),
            cell: (m) => t(m.readOnly ? `${key}.mounts.readOnly` : `${key}.mounts.readWrite`),
          },
        ]}
      />
    </SettingsCard>
  );
}

type FileRef = NonNullable<ServiceDetail["secrets"]>[number] & { kind: "secret" | "config" };

// FilesCard lists the secrets and configs the service uses, by reference: their values are
// never sent. A secret's or a config's name links to its page.
function FilesCard({ envId, detail }: { envId: string; detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  const rows: FileRef[] = [
    ...(detail.secrets ?? []).map((s) => ({ ...s, kind: "secret" as const })),
    ...(detail.configs ?? []).map((c) => ({ ...c, kind: "config" as const })),
  ];
  return (
    <SettingsCard title={t(`${key}.files.title`)}>
      <CardTable
        label={t(`${key}.files.title`)}
        empty={t(`${key}.files.empty`)}
        rows={rows}
        rowKey={(r, i) => `${i}|${r.kind}|${r.id}`}
        columns={[
          { header: t(`${key}.files.kind`), cell: (r) => t(`${key}.files.${r.kind}`) },
          {
            header: t(`${key}.files.name`),
            cell: (r) =>
              r.kind === "secret" ? (
                <Link
                  to="/env/$envId/secrets/$secretId"
                  params={{ envId, secretId: r.id }}
                  className="rounded-sm text-accent-ink hover:underline"
                >
                  {r.name}
                </Link>
              ) : (
                <Link
                  to="/env/$envId/configs/$configId"
                  params={{ envId, configId: r.id }}
                  className="rounded-sm text-accent-ink hover:underline"
                >
                  {r.name}
                </Link>
              ),
          },
          { header: t(`${key}.files.file`), cell: (r) => r.file || missing },
          {
            header: t(`${key}.files.owner`),
            cell: (r) => (r.file ? `${r.uid} / ${r.gid}` : missing),
          },
          { header: t(`${key}.files.mode`), cell: (r) => r.mode || missing },
        ]}
      />
    </SettingsCard>
  );
}

function NetworksCard({ envId, detail }: { envId: string; detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  return (
    <SettingsCard title={t(`${key}.networks.title`)}>
      <CardTable
        label={t(`${key}.networks.title`)}
        empty={t(`${key}.networks.empty`)}
        rows={detail.networks ?? []}
        rowKey={(n, i) => `${i}|${n.id}`}
        columns={[
          {
            header: t(`${key}.networks.network`),
            cell: (n) => (
              <Link
                to="/env/$envId/networks/$networkId"
                params={{ envId, networkId: n.id }}
                className="rounded-sm text-accent-ink hover:underline"
              >
                {n.name}
              </Link>
            ),
          },
          {
            header: t(`${key}.networks.aliases`),
            cell: (n) =>
              (n.aliases ?? []).length === 0 ? (
                missing
              ) : (
                <span className="flex flex-wrap gap-1">
                  {(n.aliases ?? []).map((a) => (
                    <Badge key={a} variant="secondary" className="max-w-full break-all">
                      {a}
                    </Badge>
                  ))}
                </span>
              ),
          },
        ]}
      />
    </SettingsCard>
  );
}

// EnvCard lists the variables by name. A value comes only when the server sent it, because the
// caller holds secrets.reveal; it is still hidden until the person asks to see it. Without the
// permission there is no value in the page at all (D-39).
function EnvCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const env = detail.env ?? [];
  const rows: KeyValueRow[] = env.map((v) =>
    v.masked || v.value === undefined
      ? { key: v.name, masked: true as const }
      : { key: v.name, masked: true as const, canReveal: true as const, value: v.value },
  );
  return (
    <SettingsCard title={t(`${key}.env.title`)}>
      <KeyValueTable label={t(`${key}.env.label`)} empty={t(`${key}.env.empty`)} rows={rows} />
      {detail.envMasked ? (
        <p className="text-sm text-muted-foreground">{t(`${key}.env.masked`)}</p>
      ) : null}
    </SettingsCard>
  );
}

function ResourcesCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const r = detail.resources;
  const cpu = (cores: number, unset: string) =>
    cores > 0 ? t(`${key}.resources.cpus`, { count: cores }) : unset;
  const memory = (bytes: number, unset: string) => (bytes > 0 ? formatBytes(bytes) : unset);
  const noLimit = t(`${key}.resources.noLimit`);
  const none = t(`${key}.resources.noReservation`);
  return (
    <SettingsCard title={t(`${key}.resources.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.resources.title`)}
        empty=""
        rows={[
          { key: t(`${key}.resources.cpuLimit`), value: cpu(r.cpuLimit, noLimit) },
          { key: t(`${key}.resources.cpuReservation`), value: cpu(r.cpuReservation, none) },
          { key: t(`${key}.resources.memoryLimit`), value: memory(r.memoryLimit, noLimit) },
          {
            key: t(`${key}.resources.memoryReservation`),
            value: memory(r.memoryReservation, none),
          },
        ]}
      />
    </SettingsCard>
  );
}

function PlacementCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const p = detail.placement;
  const constraints = p.constraints ?? [];
  const preferences = p.preferences ?? [];
  const rows: KeyValueRow[] = [];
  if (constraints.length > 0) {
    rows.push({
      key: t(`${key}.placement.constraints`),
      value: (
        <ul className="flex flex-col gap-1">
          {constraints.map((c) => (
            <li key={c} className="font-mono text-xs break-all">
              {c}
            </li>
          ))}
        </ul>
      ),
    });
  }
  if (preferences.length > 0) {
    rows.push({
      key: t(`${key}.placement.preferences`),
      value: (
        <ul className="flex flex-col gap-1">
          {preferences.map((label) => (
            <li key={label} className="break-all">
              {t(`${key}.placement.spread`, { label })}
            </li>
          ))}
        </ul>
      ),
    });
  }
  if (p.maxReplicas > 0) {
    rows.push({ key: t(`${key}.placement.maxReplicas`), value: String(p.maxReplicas) });
  }
  return (
    <SettingsCard title={t(`${key}.placement.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.placement.title`)}
        empty={t(`${key}.placement.empty`)}
        rows={rows}
      />
    </SettingsCard>
  );
}

function RestartCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  const p = detail.restartPolicy;
  const duration = (d: string | undefined) => (d ? humanDuration(d) : missing);
  const condition = (c: string) =>
    ["any", "on-failure", "none"].includes(c) ? t(`${key}.restart.condition.${c}`) : c;
  return (
    <SettingsCard title={t(`${key}.restart.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.restart.title`)}
        empty=""
        rows={[
          { key: t(`${key}.restart.when`), value: p?.condition ? condition(p.condition) : missing },
          { key: t(`${key}.restart.delay`), value: duration(p?.delay) },
          {
            key: t(`${key}.restart.maxAttempts`),
            value: p
              ? p.maxAttempts > 0
                ? String(p.maxAttempts)
                : t(`${key}.restart.unlimited`)
              : missing,
          },
          { key: t(`${key}.restart.window`), value: duration(p?.window) },
        ]}
      />
    </SettingsCard>
  );
}

function UpdateCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const missing = useMissing();
  const u = detail.updatePolicy;
  const duration = (d: string | undefined) => (d ? humanDuration(d) : missing);
  const word = (group: "orders" | "actions", v: string | undefined) =>
    v ? t(`${key}.update.${group}.${v}`, { defaultValue: v }) : missing;
  return (
    <SettingsCard title={t(`${key}.update.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.update.title`)}
        empty=""
        rows={[
          { key: t(`${key}.update.parallelism`), value: u ? String(u.parallelism) : missing },
          { key: t(`${key}.update.delay`), value: duration(u?.delay) },
          { key: t(`${key}.update.order`), value: word("orders", u?.order) },
          { key: t(`${key}.update.failureAction`), value: word("actions", u?.failureAction) },
          { key: t(`${key}.update.monitor`), value: duration(u?.monitor) },
          {
            key: t(`${key}.update.maxFailureRatio`),
            value: u ? String(u.maxFailureRatio) : missing,
          },
        ]}
      />
    </SettingsCard>
  );
}

function LabelsCard({ detail }: { detail: ServiceDetail }) {
  const t = useT();
  const rows = Object.entries(detail.labels ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return (
    <SettingsCard title={t(`${key}.labels.title`)}>
      <KeyValueTable
        label={t(`${key}.labels.label`)}
        empty={t(`${key}.labels.empty`)}
        rows={rows.map(([name, value]) => ({ key: name, value }))}
      />
    </SettingsCard>
  );
}

// The cards while the service is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="grid gap-4 lg:grid-cols-2" aria-busy="true">
      <span role="status" className="sr-only">
        {t("servicedetail.loading")}
      </span>
      <div className="flex flex-col gap-4">
        <Skeleton className="h-56 w-full" />
        <Skeleton className="h-40 w-full" />
      </div>
      <div className="flex flex-col gap-4">
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-56 w-full" />
      </div>
    </div>
  );
}

// ServiceOverview is the first tab of a service: everything about its spec in cards, two
// columns above 1024px. The page frame owns the load error and the gone state, so this draws
// only what a loaded service has.
export function ServiceOverview({ envId, serviceId }: ServiceOverviewProps) {
  const { data } = useQuery(serviceQuery(envId, serviceId));
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="flex flex-col gap-4">
      {canScale(data) && data.desired === 0 ? <ScaledToZero envId={envId} detail={data} /> : null}
      <div className="grid items-start gap-4 lg:grid-cols-2">
        <div className="flex min-w-0 flex-col gap-4">
          <ServiceCard detail={data} />
          <ImageCard detail={data} />
          <PortsCard detail={data} />
          <MountsCard envId={envId} detail={data} />
          <FilesCard envId={envId} detail={data} />
          <NetworksCard envId={envId} detail={data} />
        </div>
        <div className="flex min-w-0 flex-col gap-4">
          <EnvCard detail={data} />
          <ResourcesCard detail={data} />
          <PlacementCard detail={data} />
          <RestartCard detail={data} />
          <UpdateCard detail={data} />
          <LabelsCard detail={data} />
        </div>
      </div>
    </div>
  );
}
