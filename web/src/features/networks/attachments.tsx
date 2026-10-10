// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { type NetworkAttachment, networkDetailQuery } from "@/api/networks";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { ConnectDialog } from "@/features/networks/connect-dialog";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface AttachmentsProps {
  envId: string;
  networkId: string;
  // The node a local network is on, from the page's address.
  node?: string;
}

// The key of one row: a container is told apart by its node, a service by its name.
function rowKey(a: NetworkAttachment): string {
  return a.kind === "container" ? `c|${a.nodeId}|${a.id}` : `s|${a.name}`;
}

// The cells hold text from Docker. They wrap at any character, in mono, and the table
// scrolls sideways when the page is narrow.
const CELL = "align-top font-mono text-xs break-all whitespace-normal";

// Attachments is the second tab of a network: every container attached to it, on every
// node that answered, with its addresses and the names it answers to, and on a swarm
// every service whose spec mentions it. The page frame owns the load error and the
// gone state, so this draws only what a loaded network has.
export function Attachments({ envId, networkId, node }: AttachmentsProps) {
  const t = useT();
  const swarm = useEnvIsSwarm(envId);
  const { data } = useQuery(networkDetailQuery(envId, networkId, node));
  const connect = useNeeds("networks.connect", envId);
  const [connecting, setConnecting] = useState(false);
  const missing = t("networks.detail.missing");
  const key = "networks.detail.attachments";

  if (!data) {
    return (
      <div aria-busy="true">
        <span role="status" className="sr-only">
          {t(`${key}.loading`)}
        </span>
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }
  const rows = data.attachments ?? [];
  // Connect container sits above the table, and above the empty line too: an empty
  // network is where a container is most often put.
  const header = (
    <div className="flex justify-end">
      <NeedsTooltip reason={connect.reason}>
        <Button
          type="button"
          variant="outline"
          className="max-md:h-11"
          onClick={connect.allowed ? () => setConnecting(true) : undefined}
        >
          {t("networks.connect.action")}
        </Button>
      </NeedsTooltip>
      {connect.allowed ? (
        <ConnectDialog
          envId={envId}
          open={connecting}
          onOpenChange={setConnecting}
          network={{
            id: data.id,
            name: data.name,
            scope: data.scope,
            nodeId: data.nodeId,
            attachable: data.attachable,
            node,
          }}
        />
      ) : null}
    </div>
  );
  if (rows.length === 0) {
    return (
      <div className="flex flex-col gap-3">
        {header}
        <p className="text-sm text-muted-foreground">{t(`${key}.empty`)}</p>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-3">
      {header}
      {data.aliasesTruncated ? (
        <p className="text-sm text-muted-foreground">{t(`${key}.truncated`, { count: 50 })}</p>
      ) : null}
      <Table aria-label={t(`${key}.title`)}>
        <TableHeader>
          <TableRow>
            <TableHead>{t(`${key}.columns.name`)}</TableHead>
            <TableHead>{t(`${key}.columns.type`)}</TableHead>
            {swarm ? <TableHead>{t(`${key}.columns.node`)}</TableHead> : null}
            <TableHead>{t(`${key}.columns.ipv4`)}</TableHead>
            <TableHead>{t(`${key}.columns.ipv6`)}</TableHead>
            <TableHead>{t(`${key}.columns.mac`)}</TableHead>
            <TableHead>{t(`${key}.columns.aliases`)}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((a) => (
            <TableRow key={rowKey(a)}>
              <TableCell className={CELL}>
                {a.kind === "container" ? (
                  <Link
                    to="/env/$envId/containers/$containerId"
                    params={{ envId, containerId: a.id }}
                    search={{ node: a.nodeId }}
                    className="rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                  >
                    {a.name}
                  </Link>
                ) : (
                  <>
                    <span>{t(`${key}.service`, { name: a.name })}</span>
                    <span className="block text-muted-foreground">
                      {t(`${key}.tasks`, { count: a.tasks })}
                    </span>
                  </>
                )}
              </TableCell>
              <TableCell className="align-top text-sm whitespace-normal">
                {t(`${key}.type.${a.kind}`)}
              </TableCell>
              {swarm ? <TableCell className={CELL}>{a.node || missing}</TableCell> : null}
              <TableCell className={CELL}>{a.ipv4 || missing}</TableCell>
              <TableCell className={CELL}>{a.ipv6 || missing}</TableCell>
              <TableCell className={CELL}>{a.mac || missing}</TableCell>
              <TableCell className="align-top whitespace-normal">
                {(a.aliases ?? []).length > 0 ? (
                  <span className="flex flex-wrap gap-1">
                    {(a.aliases ?? []).map((alias) => (
                      <Badge
                        key={alias}
                        variant="secondary"
                        className="h-auto max-w-full font-mono text-xs break-all whitespace-normal"
                      >
                        {alias}
                      </Badge>
                    ))}
                  </span>
                ) : (
                  missing
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
