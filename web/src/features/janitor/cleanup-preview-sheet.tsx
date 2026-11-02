// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useNavigate } from "@tanstack/react-router";
import { ChevronDown } from "lucide-react";
import { Fragment, useEffect, useMemo, useRef, useState } from "react";

import {
  type JanitorItem,
  type JanitorPreview,
  JanitorRequestError,
  lockOfError,
  useJanitorPreview,
  useStartJanitorRun,
} from "@/api/janitor";
import type { StackLock } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ageText, formatSize, reasonText } from "@/features/janitor/preview-summary";
import { SelectCheckbox, TruncatedText } from "@/features/resources/resource-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";
import { cn } from "@/lib/utils";

const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// The key of one item in the ticked set: its type, its node and the engine's ID, which is also
// what the confirm sends. It covers every item the preview listed, rendered or not.
export const itemKey = (i: Pick<JanitorItem, "type" | "nodeId" | "ref">) =>
  `${i.type}|${i.nodeId}|${i.ref}`;

const TYPE_ORDER = ["container", "network", "image"] as const;

interface NodeGroupData {
  nodeId: string;
  node: string;
  items: JanitorItem[];
}

interface TypeGroupData {
  type: (typeof TYPE_ORDER)[number];
  nodes: NodeGroupData[];
  count: number;
}

// groupItems sorts the server's answer into its type sections and, inside each, its node groups. It
// keeps the server's order and never decides what may be removed. An item that belongs to no node (a
// swarm-wide network) is in a group with no node name.
function groupItems(items: readonly JanitorItem[]): TypeGroupData[] {
  const out: TypeGroupData[] = [];
  for (const type of TYPE_ORDER) {
    const own = items.filter((i) => i.type === type);
    if (own.length === 0) continue;
    const nodes: NodeGroupData[] = [];
    for (const item of own) {
      const last = nodes[nodes.length - 1];
      if (last && last.nodeId === item.nodeId) last.items.push(item);
      else nodes.push({ nodeId: item.nodeId, node: item.node, items: [item] });
    }
    out.push({ type, nodes, count: own.length });
  }
  return out;
}

interface CleanupPreviewSheetProps {
  envId: string;
  envName: string;
  // True on a swarm: items are grouped under the node they are on.
  swarm: boolean;
  // The nodes the environment should have, for the "Checking {n} nodes…" line.
  nodeCount: number;
  onOpenChange: (open: boolean) => void;
  // Called with the operation that holds the environment when Remove is refused.
  onLocked?: (lock: StackLock) => void;
}

// CleanupPreviewSheet is the dry run and its confirmation (D-14): everything the janitor would
// remove now, grouped by type and node, every item ticked, with the reason in words. Remove sends
// exactly the ticked items; the server decides again, from a fresh read, for each of them. The
// preview is asked for on open and on Refresh preview and never in the background, so the ticks are
// never reset under a person's hands.
export function CleanupPreviewSheet({
  envId,
  envName,
  swarm,
  nodeCount,
  onOpenChange,
  onLocked,
}: CleanupPreviewSheetProps) {
  const t = useT();
  const navigate = useNavigate();
  const preview = useJanitorPreview(envId);
  const start = useStartJanitorRun(envId);
  const needs = useNeeds("janitor.run", envId);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const asked = useRef(false);

  useEffect(() => {
    if (asked.current) return;
    asked.current = true;
    preview.mutate();
  }, [preview.mutate]);

  const data: JanitorPreview | undefined = preview.data;
  const items = useMemo(() => data?.items ?? [], [data]);
  const groups = useMemo(() => groupItems(items), [items]);

  // The ticks belong to one preview: a new preview (Refresh) starts again with everything ticked,
  // and an item that was not there before starts ticked too.
  const [ticks, setTicks] = useState<{ of: JanitorPreview | undefined; set: ReadonlySet<string> }>({
    of: undefined,
    set: new Set(),
  });
  const ticked = useMemo(
    () => (ticks.of === data ? ticks.set : new Set(items.map(itemKey))),
    [ticks, data, items],
  );
  function toggle(key: string) {
    const next = new Set(ticked);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    setTicks({ of: data, set: next });
  }

  const needsReason = needs.allowed
    ? undefined
    : t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName });

  async function remove() {
    if (start.isPending || ticked.size === 0) return;
    const refs = items
      .filter((i) => ticked.has(itemKey(i)))
      .map(({ type, nodeId, ref }) => ({ type, nodeId, ref }));
    try {
      const started = await start.mutateAsync(refs);
      onOpenChange(false);
      void navigate({
        to: "/env/$envId/janitor/runs/$runId",
        params: { envId, runId: started.runId },
      });
    } catch (e) {
      if (e instanceof JanitorRequestError && e.operationLocked) {
        const lock = lockOfError(e);
        onLocked?.(lock);
        toastError(
          lock.kind === "janitor"
            ? t("janitor.refused.janitor", { env: envName })
            : t("janitor.refused.deploy", { env: envName }),
        );
      } else {
        toastError(t("janitor.refused.start.title"));
      }
    }
  }

  const loading = preview.isPending || (!data && !preview.isError);
  const removeLabel = start.isPending
    ? t("janitor.preview.footer.removing")
    : t("janitor.preview.footer.remove", { count: ticked.size });

  return (
    <Sheet open onOpenChange={onOpenChange}>
      <SheetContent
        className="w-full gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-3xl"
        onOpenAutoFocus={(e) => {
          e.preventDefault();
          cancelRef.current?.focus();
        }}
      >
        <SheetHeader className="border-b pe-12">
          <SheetTitle className="break-all">
            {t("janitor.preview.title", { env: envName })}
          </SheetTitle>
          <SheetDescription>{t("janitor.preview.description")}</SheetDescription>
          {data ? (
            <p className="text-xs text-muted-foreground">
              {t("janitor.preview.summary.note", { ticked: ticked.size, total: items.length })}{" "}
              {t("janitor.preview.summary.previewed")} <RelativeTime value={data.previewedAt} />.
            </p>
          ) : null}
        </SheetHeader>
        <div className="flex min-h-0 flex-1 flex-col gap-8 overflow-y-auto px-4 py-4 md:px-6">
          {loading ? (
            <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
              <Spinner aria-hidden="true" />
              {t("janitor.preview.checking", { count: Math.max(1, nodeCount) })}
            </p>
          ) : null}
          {groups.map((group) => (
            <TypeSection
              key={group.type}
              group={group}
              swarm={swarm}
              ticked={ticked}
              onToggle={toggle}
            />
          ))}
        </div>
        <SheetFooter className="sticky bottom-0 mt-0 flex-col gap-3 border-t bg-popover md:px-6">
          <p className="text-xs text-muted-foreground">{t("janitor.preview.footer.note")}</p>
          <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
            <Button
              ref={cancelRef}
              type="button"
              variant="outline"
              className="max-md:h-11"
              onClick={() => onOpenChange(false)}
            >
              {t("janitor.preview.footer.cancel")}
            </Button>
            {data && items.length > 0 ? (
              <NeedsTooltip reason={needsReason}>
                <Button
                  type="button"
                  variant="destructive"
                  className="max-md:h-11"
                  disabled={start.isPending}
                  onClick={() => void remove()}
                >
                  {start.isPending ? <Spinner aria-hidden="true" /> : null}
                  {removeLabel}
                </Button>
              </NeedsTooltip>
            ) : null}
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}

interface TypeSectionProps {
  group: TypeGroupData;
  swarm: boolean;
  ticked: ReadonlySet<string>;
  onToggle: (key: string) => void;
}

// A type section is a collapsible card, open to start with: its title says how many there are.
function TypeSection({ group, swarm, ticked, onToggle }: TypeSectionProps) {
  const t = useT();
  return (
    <Collapsible defaultOpen className="rounded-lg border">
      <CollapsibleTrigger className="group flex h-12 w-full items-center justify-between gap-2 px-3 text-base font-semibold outline-none focus-visible:ring-3 focus-visible:ring-ring/50">
        <span>{t(`janitor.preview.sections.${group.type}`, { count: group.count })}</span>
        <ChevronDown
          aria-hidden="true"
          className="size-4 transition-transform group-data-[state=open]:rotate-180"
        />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="overflow-x-auto">
          <table className="w-full caption-bottom text-sm">
            <TableHeader>
              <TableRow>
                <TableHead className="w-11" />
                <TableHead className="min-w-48">{t("janitor.preview.columns.name")}</TableHead>
                <TableHead className="min-w-64">{t("janitor.preview.columns.reason")}</TableHead>
                <TableHead className="w-24">{t("janitor.preview.columns.age")}</TableHead>
                <TableHead className="w-24 text-end">{t("janitor.preview.columns.size")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {group.nodes.map((node) => (
                <Fragment key={node.nodeId}>
                  {swarm && node.nodeId ? (
                    <TableRow className={cn(ROW, "bg-muted/50")}>
                      <TableCell colSpan={5} className={CELL}>
                        <span className="font-mono text-xs font-semibold">{node.node}</span>{" "}
                        <span className="text-xs text-muted-foreground">
                          {t("janitor.preview.nodeGroup.items", { count: node.items.length })}
                        </span>
                      </TableCell>
                    </TableRow>
                  ) : null}
                  {node.items.map((item) => (
                    <PreviewRow
                      key={itemKey(item)}
                      item={item}
                      ticked={ticked.has(itemKey(item))}
                      onToggle={() => onToggle(itemKey(item))}
                    />
                  ))}
                </Fragment>
              ))}
            </TableBody>
          </table>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

interface PreviewRowProps {
  item: JanitorItem;
  ticked: boolean;
  onToggle: () => void;
}

// PreviewRow is one thing the janitor would remove: its name (text, never markup), the reason in
// words and how long it has waited and how much it holds.
function PreviewRow({ item, ticked, onToggle }: PreviewRowProps) {
  const t = useT();
  return (
    <TableRow className={ROW}>
      <TableCell className={cn(CELL, "w-11")}>
        <SelectCheckbox
          checked={ticked}
          label={t("janitor.preview.selectItem", { name: item.name })}
          onToggle={onToggle}
        />
      </TableCell>
      <TableCell className={cn(CELL, "min-w-48")}>
        <TruncatedText mono>{item.name}</TruncatedText>
      </TableCell>
      <TableCell className={cn(CELL, "min-w-64 whitespace-normal")}>
        {reasonText(item.reason, t)}
      </TableCell>
      <TableCell className={cn(CELL, "w-24 tabular-nums")}>
        {item.ageSeconds > 0 ? ageText(item.ageSeconds, t) : "—"}
      </TableCell>
      <TableCell className={cn(CELL, "w-24 text-end tabular-nums")}>
        {formatSize(item.sizeBytes, t)}
      </TableCell>
    </TableRow>
  );
}
