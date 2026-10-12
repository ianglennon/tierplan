// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";

import { type ImageDetail, imageQuery, imageUsedByQuery } from "@/api/images";
import { CopyableCode, copyText } from "@/components/copyable-code";
import { KeyValueTable } from "@/components/key-value-table";
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
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { toastError, toastSuccess } from "@/lib/toast";

interface ImageOverviewProps {
  envId: string;
  imageId: string;
  // The node the page reads, from its address.
  node?: string;
}

const key = "images.detail.overview";

// The cells hold text from Docker. They wrap at any character, in mono, and the table
// scrolls sideways when the page is narrow.
const CELL = "align-top font-mono text-xs break-all whitespace-normal";

// NamesCard lists every tag and digest of the image, each with its Copy.
function NamesCard({ detail }: { detail: ImageDetail }) {
  const t = useT();
  const names = detail.names ?? [];
  async function copy(name: string) {
    if (await copyText(name)) toastSuccess(t(`${key}.names.copied`, { name }));
    else toastError(t(`${key}.names.copyFailed`));
  }
  return (
    <SettingsCard title={t(`${key}.names.title`)}>
      {names.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t(`${key}.names.empty`)}</p>
      ) : (
        <Table aria-label={t(`${key}.names.label`)}>
          <TableHeader>
            <TableRow>
              <TableHead>{t(`${key}.names.columns.name`)}</TableHead>
              <TableHead>{t(`${key}.names.columns.type`)}</TableHead>
              <TableHead className="w-0">
                <span className="sr-only">{t(`${key}.names.copy`)}</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {names.map((n) => (
              <TableRow key={`${n.kind}|${n.name}`}>
                <TableCell className={CELL}>{n.name}</TableCell>
                <TableCell className="align-top">
                  <Badge variant="outline">{t(`${key}.names.type.${n.kind}`)}</Badge>
                </TableCell>
                <TableCell className="align-top">
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="max-md:h-11"
                    aria-label={t(`${key}.names.copyLabel`, { name: n.name })}
                    onClick={() => void copy(n.name)}
                  >
                    {t(`${key}.names.copy`)}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </SettingsCard>
  );
}

// DetailsCard is the image's facts: what it is and what it is built for.
function DetailsCard({ detail }: { detail: ImageDetail }) {
  const t = useT();
  const missing = t("dashboard.time.missing");
  const arch = detail.architecture
    ? detail.variant
      ? `${detail.architecture}/${detail.variant}`
      : detail.architecture
    : missing;
  return (
    <SettingsCard title={t(`${key}.details.title`)}>
      <KeyValueTable
        mono={false}
        label={t(`${key}.details.title`)}
        empty=""
        rows={[
          {
            key: t(`${key}.details.id`),
            value: <CopyableCode code={detail.id} label={t(`${key}.details.idLabel`)} />,
          },
          { key: t(`${key}.details.size`), value: formatBytes(detail.size) || missing },
          { key: t(`${key}.details.created`), value: <RelativeTime value={detail.created} /> },
          { key: t(`${key}.details.os`), value: detail.os || missing },
          { key: t(`${key}.details.architecture`), value: arch },
        ]}
      />
    </SettingsCard>
  );
}

// NodesCard says, on a swarm, which nodes have the image and under which names.
function NodesCard({ envId, detail, node }: { envId: string; detail: ImageDetail; node?: string }) {
  const t = useT();
  // How many containers each node has made from the image, running or not. They come from the
  // Used by read, which the page shares; until it answers, or when it fails, the cell is a dash.
  const usage = useQuery(imageUsedByQuery(envId, detail.id, node)).data;
  const containers = new Map<string, number>();
  for (const u of usage?.items ?? []) {
    if (u.kind === "container") containers.set(u.nodeId, (containers.get(u.nodeId) ?? 0) + 1);
  }
  return (
    <SettingsCard title={t(`${key}.nodes.title`)}>
      <Table aria-label={t(`${key}.nodes.title`)}>
        <TableHeader>
          <TableRow>
            <TableHead>{t(`${key}.nodes.columns.node`)}</TableHead>
            <TableHead className="text-right">{t(`${key}.nodes.columns.containers`)}</TableHead>
            <TableHead>{t(`${key}.nodes.columns.names`)}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(detail.nodes ?? []).map((n) => (
            <TableRow key={n.nodeId}>
              <TableCell className="align-top break-words whitespace-normal">{n.name}</TableCell>
              <TableCell className="text-right align-top tabular-nums">
                {!n.present || !usage ? (
                  <span className="text-muted-foreground">{t("dashboard.time.missing")}</span>
                ) : (
                  (containers.get(n.nodeId) ?? 0)
                )}
              </TableCell>
              <TableCell className={CELL}>
                {!n.present ? (
                  <span className="font-sans text-muted-foreground">
                    {t(`${key}.nodes.absent`)}
                  </span>
                ) : (n.names ?? []).length === 0 ? (
                  <span className="font-sans text-muted-foreground">
                    {t(`${key}.nodes.noNames`)}
                  </span>
                ) : (
                  <ul className="flex flex-col gap-1">
                    {(n.names ?? []).map((name) => (
                      <li key={name}>{name}</li>
                    ))}
                  </ul>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </SettingsCard>
  );
}

function LabelsCard({ detail }: { detail: ImageDetail }) {
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

// The cards while the image is read, in the layout they take afterwards.
function OverviewSkeleton() {
  const t = useT();
  return (
    <div className="grid items-start gap-6 lg:grid-cols-2" aria-busy="true">
      <span role="status" className="sr-only">
        {t(`${key}.loading`)}
      </span>
      <div className="flex min-w-0 flex-col gap-6">
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-56 w-full" />
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        <Skeleton className="h-32 w-full" />
      </div>
    </div>
  );
}

// ImageOverview is the first tab of an image: a two-column grid of cards above 1024px, Names
// and Details, then On nodes (swarm only) and Labels; one column below. The page frame owns
// the load error and the gone state, so this draws only what a loaded image has.
export function ImageOverview({ envId, imageId, node }: ImageOverviewProps) {
  const swarm = useEnvIsSwarm(envId);
  const { data } = useQuery(imageQuery(envId, imageId, node));
  if (!data) return <OverviewSkeleton />;
  return (
    <div className="grid items-start gap-6 lg:grid-cols-2">
      <div className="flex min-w-0 flex-col gap-6">
        <NamesCard detail={data} />
        <DetailsCard detail={data} />
      </div>
      <div className="flex min-w-0 flex-col gap-6">
        {swarm ? <NodesCard envId={envId} detail={data} node={node} /> : null}
        <LabelsCard detail={data} />
      </div>
    </div>
  );
}
