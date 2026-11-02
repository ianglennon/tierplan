// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import type { ImageRow } from "@/api/resources";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { useT } from "@/i18n/use-t";

// How long the pointer may leave the button for the popover before it closes, so crossing
// the gap between them does not shut it.
const CLOSE_DELAY_MS = 120;

interface ImageNodesCellProps {
  // The merged row: the nodes that hold the image, each with its container count.
  item: ImageRow;
  // Every node that answered, so the nodes without the image can be named.
  nodeRefs: readonly { nodeId: string; name: string }[];
  // How many nodes the environment has, answered or not.
  nodeCount: number;
  // The nodes that did not answer: they may hold the image, so they read Missing too.
  notReporting?: readonly string[] | null;
}

// ImageNodesCell is the Nodes cell of a merged swarm row: "on 2/3 nodes", or "on all 3 nodes"
// when every node has the image, as a button that opens a popover listing every node, with
// its container count where it has the image and Missing in muted where it does not. The
// popover opens on hover and on a click or the keyboard (D-16).
export function ImageNodesCell({ item, nodeRefs, nodeCount, notReporting }: ImageNodesCellProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const holders = new Map((item.nodes ?? []).map((n) => [n.nodeId, n]));
  const present = holders.size;
  const total = Math.max(nodeCount, present);
  const label =
    present === total
      ? t("images.list.nodes.all", { count: total })
      : t("images.list.nodes.some", { present, total });

  function hover(next: boolean) {
    clearTimeout(timer.current);
    if (next) setOpen(true);
    else timer.current = setTimeout(() => setOpen(false), CLOSE_DELAY_MS);
  }

  const missing = [
    ...nodeRefs.filter((n) => !holders.has(n.nodeId)).map((n) => n.name),
    ...(notReporting ?? []),
  ];
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="link"
          size="sm"
          className="h-auto p-0 tabular-nums max-md:min-h-11"
          onMouseEnter={() => hover(true)}
          onMouseLeave={() => hover(false)}
        >
          {label}
        </Button>
      </PopoverTrigger>
      <PopoverContent
        align="start"
        className="w-80"
        onMouseEnter={() => hover(true)}
        onMouseLeave={() => hover(false)}
      >
        <p className="font-medium">{t("images.list.nodes.title")}</p>
        <ul className="flex flex-col gap-1">
          {(item.nodes ?? []).map((n) => (
            <li key={n.nodeId} className="flex items-baseline justify-between gap-3">
              <span className="min-w-0 break-words">{n.name}</span>
              <span className="shrink-0 text-muted-foreground tabular-nums">
                {t("images.list.nodes.containers", { count: n.containers })}
              </span>
            </li>
          ))}
          {missing.map((name) => (
            <li key={`missing|${name}`} className="flex items-baseline justify-between gap-3">
              <span className="min-w-0 break-words">{name}</span>
              <span className="shrink-0 text-muted-foreground">
                {t("images.list.nodes.missing")}
              </span>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
