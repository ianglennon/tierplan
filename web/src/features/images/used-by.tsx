// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";

import { type ImageUsedByItem, imageUsedByQuery } from "@/api/images";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { useT } from "@/i18n/use-t";

interface ImageUsedByProps {
  envId: string;
  imageId: string;
  // The node the page reads, from its address.
  node?: string;
}

const key = "images.detail.usedBy";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// usedWhat words what uses an image: "2 containers", or "2 containers, 1 service" on a swarm,
// where a service can name an image. The caller says "Not in use" for none.
export function usedWhat(
  t: Translate,
  containers: number,
  services: number,
  swarm: boolean,
): string {
  const c = t("images.used.containers", { count: containers });
  if (!swarm) return c;
  return t("images.used.both", {
    containers: c,
    services: t("images.used.services", { count: services }),
  });
}

// The cells hold text from Docker. They wrap at any character, in mono, and the table
// scrolls sideways when the page is narrow.
const CELL = "align-top font-mono text-xs break-all whitespace-normal";

function rowKey(u: ImageUsedByItem): string {
  return u.kind === "container" ? `c|${u.nodeId}|${u.id}` : `s|${u.id || u.name}`;
}

// ImageUsedBy is the Used by tab: every container, running or stopped, created from the image
// on a node that has it, and on a swarm every service whose spec names it, even with no task.
// A container's name opens its page; a service's name is text until services have pages. The
// page frame owns the gone state and the in-use callout.
export function ImageUsedBy({ envId, imageId, node }: ImageUsedByProps) {
  const t = useT();
  const missing = t("dashboard.time.missing");
  const query = useQuery(imageUsedByQuery(envId, imageId, node));
  if (query.data) {
    const rows = query.data.items ?? [];
    if (rows.length === 0)
      return <p className="text-sm text-muted-foreground">{t(`${key}.empty`)}</p>;
    return (
      <section
        // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
        tabIndex={0}
        aria-label={t(`${key}.region`)}
        className="overflow-auto rounded-lg border focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <Table aria-label={t(`${key}.label`)}>
          <TableHeader>
            <TableRow>
              <TableHead>{t(`${key}.columns.name`)}</TableHead>
              <TableHead>{t(`${key}.columns.type`)}</TableHead>
              <TableHead>{t(`${key}.columns.node`)}</TableHead>
              <TableHead>{t(`${key}.columns.state`)}</TableHead>
              <TableHead>{t(`${key}.columns.matchedBy`)}</TableHead>
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
                      aria-label={t(`${key}.openContainer`, { name: u.name })}
                      className="rounded-sm text-accent-ink hover:underline"
                    >
                      {u.name}
                    </Link>
                  ) : (
                    u.name
                  )}
                </TableCell>
                <TableCell className="align-top">
                  <Badge variant="outline">{t(`${key}.type.${u.kind}`)}</Badge>
                </TableCell>
                <TableCell className={CELL}>{u.node || missing}</TableCell>
                <TableCell className="align-top">
                  {u.state === "no_tasks" ? t(`${key}.noTasks`) : u.state || missing}
                </TableCell>
                <TableCell className="align-top">{t(`${key}.matched.${u.matchedBy}`)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </section>
    );
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t(`${key}.loadFailed`)}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t(`${key}.retry`)}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t(`${key}.loading`)}
      </span>
      <Skeleton className="h-40 w-full" />
    </div>
  );
}
