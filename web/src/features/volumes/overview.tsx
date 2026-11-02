// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import {
  sizeKey,
  useVolumeSizes,
  type VolumeDetail,
  type VolumeUsedBy,
  volumeDetailQuery,
} from "@/api/volumes";
import { CopyableCode } from "@/components/copyable-code";
import { KeyValueTable } from "@/components/key-value-table";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SettingsCard } from "@/features/environments/settings-card";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

interface VolumeOverviewProps {
  envId: string;
  volumeName: string;
  // The node the volume is on, from the page's address.
  node?: string;
}

const key = "volumes.detail.overview";

// SizeValue is the Size row: the size when it is known, else the Calculate size button.
// The engine walks the volume's files to answer, so nothing asks until it is pressed.
function SizeValue({ envId, detail }: { envId: string; detail: VolumeDetail }) {
  const t = useT();
  const sizing = useVolumeSizes(envId);
  const missing = t("dashboard.time.missing");
  const size = sizing.data?.sizes.get(sizeKey(detail.nodeId, detail.name));
  const failedNode = sizing.data?.failed.get(detail.nodeId);
  if (size !== undefined) return <span className="tabular-nums">{formatBytes(size)}</span>;
  return (
    <span className="flex flex-wrap items-center gap-3">
      {failedNode !== undefined ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              className="cursor-help rounded-sm focus-visible:ring-2 focus-visible:ring-ring"
              aria-label={`${missing} ${t("volumes.sizes.failedOn", { node: failedNode })}`}
            >
              {missing}
            </button>
          </TooltipTrigger>
          <TooltipContent>{t("volumes.sizes.failedOn", { node: failedNode })}</TooltipContent>
        </Tooltip>
      ) : (
        <span>{missing}</span>
      )}
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="max-md:h-11"
        disabled={sizing.isPending}
        onClick={() => sizing.mutate(detail.nodeId)}
      >
        {sizing.isPending ? <Spinner aria-hidden="true" /> : null}
        {sizing.isPending ? t(`${key}.details.calculating`) : t(`${key}.details.calculate`)}
      </Button>
    </span>
  );
}

// DetailsCard is the volume's facts: where it is and what it is.
function DetailsCard({ envId, detail }: { envId: string; detail: VolumeDetail }) {
  const t = useT();
  const swarm = useEnvIsSwarm(envId);
  const missing = t("dashboard.time.missing");
  return (
    <SettingsCard title={t(`${key}.details.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.details.title`)}
        empty=""
        rows={[
          {
            key: t(`${key}.details.name`),
            value: (
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="block max-w-full truncate font-mono text-xs">{detail.name}</span>
                </TooltipTrigger>
                <TooltipContent className="break-all">{detail.name}</TooltipContent>
              </Tooltip>
            ),
          },
          { key: t(`${key}.details.driver`), value: detail.driver || missing },
          ...(swarm ? [{ key: t(`${key}.details.node`), value: detail.node || missing }] : []),
          {
            key: t(`${key}.details.mountpoint`),
            value: detail.mountpoint ? (
              <CopyableCode code={detail.mountpoint} label={t(`${key}.details.mountpointLabel`)} />
            ) : (
              missing
            ),
          },
          { key: t(`${key}.details.scope`), value: detail.scope || missing },
          { key: t(`${key}.details.created`), value: <RelativeTime value={detail.created} /> },
          { key: t(`${key}.details.size`), value: <SizeValue envId={envId} detail={detail} /> },
        ]}
      />
    </SettingsCard>
  );
}

// The cells hold text from Docker. They wrap at any character, in mono, and the table
// scrolls sideways when the page is narrow.
const CELL = "align-top font-mono text-xs break-all whitespace-normal";

function rowKey(u: VolumeUsedBy): string {
  return u.kind === "container" ? `c|${u.nodeId}|${u.id}` : `s|${u.id || u.name}`;
}

// UsedByCard is what mounts the volume: every container on its node, running or not, and
// on a swarm every service whose spec mounts it. A container's name opens its page.
function UsedByCard({ envId, detail }: { envId: string; detail: VolumeDetail }) {
  const t = useT();
  const missing = t("dashboard.time.missing");
  const rows = detail.usedBy ?? [];
  return (
    <SettingsCard title={t(`${key}.usedBy.title`)}>
      {rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t(`${key}.usedBy.empty`)}</p>
      ) : (
        <Table aria-label={t(`${key}.usedBy.title`)}>
          <TableHeader>
            <TableRow>
              <TableHead>{t(`${key}.usedBy.columns.name`)}</TableHead>
              <TableHead>{t(`${key}.usedBy.columns.type`)}</TableHead>
              <TableHead>{t(`${key}.usedBy.columns.node`)}</TableHead>
              <TableHead>{t(`${key}.usedBy.columns.mountedAt`)}</TableHead>
              <TableHead>{t(`${key}.usedBy.columns.mode`)}</TableHead>
              <TableHead>{t(`${key}.usedBy.columns.state`)}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((u) => (
              <TableRow key={rowKey(u)}>
                <TableCell className={CELL}>
                  {u.kind === "container" ? (
                    <Link
                      to="/env/$envId/containers/$containerId"
                      params={{ envId, containerId: u.id }}
                      search={{ node: u.nodeId }}
                      aria-label={t(`${key}.usedBy.openContainer`, { name: u.name })}
                      className="rounded-sm text-accent-ink hover:underline"
                    >
                      {u.name}
                    </Link>
                  ) : (
                    u.name
                  )}
                </TableCell>
                <TableCell className="align-top">
                  <Badge variant="outline">{t(`${key}.usedBy.type.${u.kind}`)}</Badge>
                </TableCell>
                <TableCell className={CELL}>{u.node || missing}</TableCell>
                <TableCell className={CELL}>{u.mountedAt || missing}</TableCell>
                <TableCell className="align-top">
                  {t(u.readOnly ? `${key}.usedBy.readOnly` : `${key}.usedBy.readWrite`)}
                </TableCell>
                <TableCell className="align-top">{u.state || missing}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </SettingsCard>
  );
}

// OptionsCard lists the driver options. The server withholds the values from a caller
// without secrets.reveal, and sends them to one who has it; those stay hidden until the
// person shows them.
function OptionsCard({ detail }: { detail: VolumeDetail }) {
  const t = useT();
  const options = detail.options ?? [];
  return (
    <SettingsCard title={t(`${key}.options.title`)}>
      <KeyValueTable
        label={t(`${key}.options.label`)}
        empty={t(`${key}.options.empty`)}
        rows={options.map((o) =>
          o.masked || o.value === undefined
            ? { key: o.name, masked: true as const }
            : { key: o.name, masked: true as const, canReveal: true as const, value: o.value },
        )}
      />
      {options.some((o) => o.masked) ? (
        <p className="text-sm text-muted-foreground">{t(`${key}.options.masked`)}</p>
      ) : null}
    </SettingsCard>
  );
}

function LabelsCard({ detail }: { detail: VolumeDetail }) {
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

// The cards while the volume is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-4" aria-busy="true">
      <span role="status" className="sr-only">
        {t(`${key}.loading`)}
      </span>
      <Skeleton className="h-56 w-full" />
      <Skeleton className="h-32 w-full" />
      <Skeleton className="h-24 w-full" />
    </div>
  );
}

// VolumeOverview is the first tab of a volume: one column of cards, Details, Used by,
// Options and Labels. The page frame owns the load error and the gone state, so this
// draws only what a loaded volume has.
export function VolumeOverview({ envId, volumeName, node }: VolumeOverviewProps) {
  const { data } = useQuery(volumeDetailQuery(envId, volumeName, node));
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="mx-auto flex w-full max-w-4xl flex-col gap-4">
      <DetailsCard envId={envId} detail={data} />
      <UsedByCard envId={envId} detail={data} />
      <OptionsCard detail={data} />
      <LabelsCard detail={data} />
    </div>
  );
}
