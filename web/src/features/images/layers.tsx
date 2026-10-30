// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { useLayoutEffect, useRef, useState } from "react";

import { type ImageLayer, imageHistoryQuery, isAgentTooOld } from "@/api/images";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription } from "@/components/ui/alert";
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
import { formatBytes } from "@/lib/format";
import { cn } from "@/lib/utils";

interface ImageLayersProps {
  envId: string;
  imageId: string;
  // The node the page reads, from its address.
  node?: string;
}

const key = "images.detail.layers";

// layerId is the first 12 characters of a layer's ID as `docker history` shows it, or a dash
// for a layer of a pulled image, which the engine calls <missing>, and for one with no ID.
function layerId(id: string): string {
  const hex = id.startsWith("sha256:") ? id.slice("sha256:".length) : "";
  return hex ? hex.slice(0, 12) : "—";
}

// LayerCommand is the command that made a layer: text as Docker reports it, in mono, cut at
// 3 lines with Show more when it is longer. Whether it is cut is measured, since only the
// browser knows how the text wraps.
function LayerCommand({ text }: { text: string }) {
  const t = useT();
  const ref = useRef<HTMLDivElement>(null);
  const [expanded, setExpanded] = useState(false);
  const [overflows, setOverflows] = useState(false);

  // biome-ignore lint/correctness/useExhaustiveDependencies: text is what is measured
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el) return;
    const measure = () => {
      // Once expanded the box is as tall as the text; the toggle stays until it is closed.
      if (!expanded) setOverflows(el.scrollHeight > el.clientHeight + 1);
    };
    measure();
    if (typeof ResizeObserver === "undefined") return;
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [text, expanded]);

  if (!text) return <span className="text-muted-foreground">—</span>;
  return (
    <div className="flex flex-col items-start gap-1">
      <div
        ref={ref}
        data-slot="layer-command"
        className={cn(
          "max-w-full font-mono text-xs break-all whitespace-pre-wrap",
          !expanded && "line-clamp-3",
        )}
      >
        {text}
      </div>
      {overflows || expanded ? (
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto p-0 max-md:min-h-11"
          aria-expanded={expanded}
          onClick={() => setExpanded((v) => !v)}
        >
          {expanded ? t(`${key}.showLess`) : t(`${key}.showMore`)}
        </Button>
      ) : null}
    </div>
  );
}

function LayersTable({ layers, masked }: { layers: ImageLayer[]; masked: boolean }) {
  const t = useT();
  const total = layers.reduce((sum, l) => sum + l.size, 0);
  return (
    <div className="flex flex-col gap-3">
      {masked ? (
        <Alert>
          <AlertDescription>{t(`${key}.masked`)}</AlertDescription>
        </Alert>
      ) : null}
      {/* The scrolling region is focusable, so a keyboard user can scroll it. */}
      <section
        // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
        tabIndex={0}
        aria-label={t(`${key}.region`)}
        className="max-h-[70vh] overflow-auto rounded-lg border focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        <Table aria-label={t(`${key}.label`)}>
          <TableHeader>
            <TableRow>
              <TableHead className="w-32">{t(`${key}.columns.layer`)}</TableHead>
              <TableHead>{t(`${key}.columns.createdBy`)}</TableHead>
              <TableHead className="w-28 text-right">{t(`${key}.columns.size`)}</TableHead>
              <TableHead className="w-32">{t(`${key}.columns.created`)}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {layers.map((l, i) => (
              // The engine gives no unique key: pulled layers all read <missing>.
              // biome-ignore lint/suspicious/noArrayIndexKey: the list is read-only and ordered
              <TableRow key={i}>
                <TableCell className="align-top font-mono text-xs">{layerId(l.id)}</TableCell>
                <TableCell className="align-top whitespace-normal">
                  <LayerCommand text={l.createdBy} />
                </TableCell>
                <TableCell className="text-right align-top tabular-nums">
                  {l.size === 0 ? (
                    <span className="text-muted-foreground">{formatBytes(0)}</span>
                  ) : (
                    formatBytes(l.size)
                  )}
                </TableCell>
                <TableCell className="align-top">
                  <RelativeTime value={l.created} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </section>
      <p className="text-sm text-muted-foreground">
        {t(`${key}.caption`, { count: layers.length, size: formatBytes(total) })}
      </p>
    </div>
  );
}

// ImageLayers is the Layers tab: the image's history, one row per layer, newest first. An
// agent too old to know the history route shows the agent-update notice in place of the table,
// and the tab stays (UI-SPEC "Image detail"). The page frame owns the gone state.
export function ImageLayers({ envId, imageId, node }: ImageLayersProps) {
  const t = useT();
  const query = useQuery(imageHistoryQuery(envId, imageId, node));
  if (query.data) {
    const layers = query.data.layers ?? [];
    if (layers.length === 0)
      return <p className="text-sm text-muted-foreground">{t(`${key}.empty`)}</p>;
    return <LayersTable layers={layers} masked={query.data.masked === true} />;
  }
  if (query.isError) {
    if (isAgentTooOld(query.error)) return <AgentUpdateNotice envId={envId} />;
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
      <Skeleton className="h-80 w-full" />
    </div>
  );
}
