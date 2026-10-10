// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";

import { type ContainerDetail, containerDetailQuery } from "@/api/container-detail";
import { type KeyValueRow, KeyValueTable } from "@/components/key-value-table";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
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
import { ConnectDialog } from "@/features/networks/connect-dialog";
import {
  DisconnectDialog,
  type DisconnectDialogTarget,
} from "@/features/networks/disconnect-dialog";
import { useNeeds } from "@/hooks/use-needs";
import i18n from "@/i18n";
import { useT } from "@/i18n/use-t";

interface OverviewProps {
  envId: string;
  containerId: string;
  // The node the container is on, from the page's address.
  node?: string;
}

// StateCard is the State rows: where the container is in its life.
function StateCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  return (
    <SettingsCard title={t("containerdetail.state.title")}>
      <KeyValueTable
        mono={false}
        label={t("containerdetail.state.title")}
        empty=""
        rows={[
          {
            key: t("containerdetail.state.status"),
            value: t(`dashboard.lists.states.${detail.state}`),
          },
          {
            key: t("containerdetail.state.health"),
            value: t(`containerdetail.health.${detail.health}`),
          },
          {
            key: t("containerdetail.state.started"),
            value: <RelativeTime value={detail.startedAt} />,
          },
          {
            key: t("containerdetail.state.finished"),
            value: <RelativeTime value={detail.finishedAt} />,
          },
          { key: t("containerdetail.state.exitCode"), value: String(detail.exitCode) },
          { key: t("containerdetail.state.restarts"), value: String(detail.restartCount) },
          {
            key: t("containerdetail.state.oomKilled"),
            value: t(detail.oomKilled ? "containerdetail.state.yes" : "containerdetail.state.no"),
          },
        ]}
      />
    </SettingsCard>
  );
}

// What a row shows for a value that is not set.
function useMissing() {
  const t = useT();
  return t("containerdetail.missing");
}

// ImageCard is the image and how the container starts. Long commands wrap in mono
// rather than truncate, so they can be read and copied whole. A command line can
// carry a password, so the server withholds it from a caller without secrets.reveal
// (commandMasked); it is then drawn as a masked value, as an environment value is.
function ImageCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  const missing = useMissing();
  const text = (v: string | undefined) => v || missing;
  const argvRow = (key: string, argv: string[] | null | undefined): KeyValueRow =>
    detail.commandMasked && (argv ?? []).length > 0
      ? { key, masked: true }
      : { key, value: text((argv ?? []).join(" ")) };
  return (
    <SettingsCard title={t("containerdetail.imageCommand.title")}>
      <KeyValueTable
        label={t("containerdetail.imageCommand.title")}
        empty=""
        rows={[
          { key: t("containerdetail.imageCommand.image"), value: text(detail.image) },
          argvRow(t("containerdetail.imageCommand.command"), detail.command),
          argvRow(t("containerdetail.imageCommand.entrypoint"), detail.entrypoint),
          { key: t("containerdetail.imageCommand.workingDir"), value: text(detail.workingDir) },
          { key: t("containerdetail.imageCommand.user"), value: text(detail.user) },
        ]}
      />
      {detail.commandMasked ? (
        <p className="text-sm text-muted-foreground">{t("containerdetail.imageCommand.masked")}</p>
      ) : null}
    </SettingsCard>
  );
}

// RestartCard words the restart policy; "no" is Never restart, and the maximum
// shows only when the container sets one.
function RestartCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  const key = `containerdetail.restart.policy.${detail.restartPolicy}`;
  return (
    <SettingsCard title={t("containerdetail.restart.title")}>
      <div className="flex flex-col gap-1 text-sm">
        <span>{i18n.exists(key, { ns: "containerdetail" }) ? t(key) : detail.restartPolicy}</span>
        {detail.restartMaxRetries > 0 ? (
          <span className="text-muted-foreground">
            {t("containerdetail.restart.maxRetries", { count: detail.restartMaxRetries })}
          </span>
        ) : null}
      </div>
    </SettingsCard>
  );
}

interface GridColumn<T> {
  header: string;
  cell: (row: T) => ReactNode;
  // A column of actions has a name for assistive technology and none on screen.
  hideHeader?: boolean;
}

// A table inside a card: it scrolls sideways when the card is narrow, and its
// text wraps at any character, in mono. Everything in it is text from Docker.
function CardTable<T>({
  label,
  columns,
  rows,
  rowKey,
  empty,
}: {
  label: string;
  columns: readonly GridColumn<T>[];
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
            <TableHead key={c.header}>
              {c.hideHeader ? <span className="sr-only">{c.header}</span> : c.header}
            </TableHead>
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

// The host side of a published port. An IPv6 address needs its brackets before the
// port.
function hostOf(p: NonNullable<ContainerDetail["ports"]>[number]): string {
  if (!p.hostIp) return p.hostPort;
  return `${p.hostIp.includes(":") ? `[${p.hostIp}]` : p.hostIp}:${p.hostPort}`;
}

function PortsCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  return (
    <SettingsCard title={t("containerdetail.ports.title")}>
      <CardTable
        label={t("containerdetail.ports.title")}
        empty={t("containerdetail.ports.empty")}
        rows={detail.ports ?? []}
        rowKey={(p, i) => `${i}|${p.protocol}|${p.containerPort}|${hostOf(p)}`}
        columns={[
          {
            header: t("containerdetail.ports.containerPort"),
            cell: (p) => String(p.containerPort),
          },
          { header: t("containerdetail.ports.host"), cell: (p) => hostOf(p) },
          { header: t("containerdetail.ports.protocol"), cell: (p) => p.protocol },
        ]}
      />
    </SettingsCard>
  );
}

function MountsCard({ envId, detail }: { envId: string; detail: ContainerDetail }) {
  const t = useT();
  const missing = useMissing();
  return (
    <SettingsCard title={t("containerdetail.mounts.title")}>
      <CardTable
        label={t("containerdetail.mounts.title")}
        empty={t("containerdetail.mounts.empty")}
        rows={detail.mounts ?? []}
        rowKey={(m, i) => `${i}|${m.destination}`}
        columns={[
          { header: t("containerdetail.mounts.type"), cell: (m) => m.type },
          // A volume is named; where the engine keeps it is not what a person looks for. Its
          // name opens the volume's page on this container's node, since a name repeats per
          // node.
          {
            header: t("containerdetail.mounts.source"),
            cell: (m) =>
              m.type === "volume" && m.name ? (
                <Link
                  to="/env/$envId/volumes/$volumeName"
                  params={{ envId, volumeName: m.name }}
                  search={{ node: detail.nodeId }}
                  className="rounded-sm text-accent-ink hover:underline"
                >
                  {m.name}
                </Link>
              ) : (
                m.source || missing
              ),
          },
          { header: t("containerdetail.mounts.destination"), cell: (m) => m.destination },
          {
            header: t("containerdetail.mounts.mode"),
            cell: (m) =>
              [
                t(m.rw ? "containerdetail.mounts.readWrite" : "containerdetail.mounts.readOnly"),
                m.mode,
              ]
                .filter(Boolean)
                .join(", "),
          },
        ]}
      />
    </SettingsCard>
  );
}

// NetworksCard shows where the container is attached, with the means to change it:
// "Connect to network" above the table and "Disconnect…" on each row, both for an
// Operator (D-43, D-51) and disabled with the reason for everyone else.
function NetworksCard({ envId, detail }: { envId: string; detail: ContainerDetail }) {
  const t = useT();
  const missing = useMissing();
  const connect = useNeeds("networks.connect", envId);
  const [connecting, setConnecting] = useState(false);
  const [disconnecting, setDisconnecting] = useState<DisconnectDialogTarget | null>(null);
  return (
    <SettingsCard title={t("containerdetail.networks.title")}>
      <div className="flex justify-end">
        <NeedsTooltip reason={connect.reason}>
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            onClick={connect.allowed ? () => setConnecting(true) : undefined}
          >
            {t("networks.connect.actionContainer")}
          </Button>
        </NeedsTooltip>
      </div>
      <CardTable
        label={t("containerdetail.networks.title")}
        empty={t("containerdetail.networks.empty")}
        rows={detail.networks ?? []}
        rowKey={(n) => n.networkId || n.name}
        columns={[
          { header: t("containerdetail.networks.network"), cell: (n) => n.name },
          { header: t("containerdetail.networks.ipv4"), cell: (n) => n.ipv4 || missing },
          { header: t("containerdetail.networks.ipv6"), cell: (n) => n.ipv6 || missing },
          { header: t("containerdetail.networks.mac"), cell: (n) => n.mac || missing },
          {
            header: t("containerdetail.networks.aliases"),
            cell: (n) => (n.aliases ?? []).join(", ") || missing,
          },
          {
            header: t("networks.disconnect.actions"),
            hideHeader: true,
            cell: (n) => (
              <NeedsTooltip reason={connect.reason}>
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="max-md:h-11"
                  aria-label={t("networks.disconnect.label", {
                    container: detail.name,
                    network: n.name,
                  })}
                  onClick={
                    connect.allowed
                      ? () =>
                          setDisconnecting({
                            networkId: n.networkId,
                            networkName: n.name,
                            containerId: detail.id,
                            containerName: detail.name,
                            nodeId: detail.nodeId,
                          })
                      : undefined
                  }
                >
                  {t("networks.disconnect.action")}
                </Button>
              </NeedsTooltip>
            ),
          },
        ]}
      />
      {connect.allowed ? (
        <>
          <ConnectDialog
            envId={envId}
            open={connecting}
            onOpenChange={setConnecting}
            container={{ id: detail.id, name: detail.name, nodeId: detail.nodeId }}
          />
          <DisconnectDialog
            envId={envId}
            target={disconnecting}
            onClose={() => setDisconnecting(null)}
          />
        </>
      ) : null}
    </SettingsCard>
  );
}

function LabelsCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  const rows = Object.entries(detail.labels ?? {}).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return (
    <SettingsCard title={t("containerdetail.labels.title")}>
      <KeyValueTable
        label={t("containerdetail.labels.label")}
        empty={t("containerdetail.labels.empty")}
        rows={rows.map(([key, value]) => ({ key, value }))}
      />
    </SettingsCard>
  );
}

// EnvCard lists the variables by name. A value comes only when the server sent it,
// because the caller holds secrets.reveal; it is still hidden until the person asks
// to see it. Without the permission there is no value in the page at all.
function EnvCard({ detail }: { detail: ContainerDetail }) {
  const t = useT();
  const env = detail.env ?? [];
  const anyMasked = env.some((v) => v.masked);
  return (
    <SettingsCard title={t("containerdetail.env.title")}>
      <KeyValueTable
        label={t("containerdetail.env.label")}
        empty={t("containerdetail.env.empty")}
        rows={env.map((v) =>
          v.masked || v.value === undefined
            ? { key: v.name, masked: true as const }
            : { key: v.name, masked: true as const, canReveal: true as const, value: v.value },
        )}
      />
      {anyMasked ? (
        <p className="text-sm text-muted-foreground">{t("containerdetail.env.masked")}</p>
      ) : null}
    </SettingsCard>
  );
}

// The cards while the container is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="grid gap-4 lg:grid-cols-2" aria-busy="true">
      <span role="status" className="sr-only">
        {t("containerdetail.loading")}
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

// Overview is the first tab of a container: everything about it in cards, two
// columns above 1024px (D-04). The page frame owns the load error and the gone
// state, so this draws only what a loaded container has.
export function Overview({ envId, containerId, node }: OverviewProps) {
  const { data } = useQuery(containerDetailQuery(envId, containerId, node));
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="grid items-start gap-4 lg:grid-cols-2">
      <div className="flex min-w-0 flex-col gap-4">
        <StateCard detail={data} />
        <ImageCard detail={data} />
        <RestartCard detail={data} />
        <PortsCard detail={data} />
        <MountsCard envId={envId} detail={data} />
      </div>
      <div className="flex min-w-0 flex-col gap-4">
        <NetworksCard envId={envId} detail={data} />
        <LabelsCard detail={data} />
        <EnvCard detail={data} />
      </div>
    </div>
  );
}
