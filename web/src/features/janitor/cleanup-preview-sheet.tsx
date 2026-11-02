// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useNavigate } from "@tanstack/react-router";
import { useVirtualizer } from "@tanstack/react-virtual";
import { ChevronDown, Info } from "lucide-react";
import { type ReactNode, type RefObject, useEffect, useMemo, useRef, useState } from "react";

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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ageText, formatSize, reasonText, summaryLine } from "@/features/janitor/preview-summary";
import { SelectCheckbox, TruncatedText } from "@/features/resources/resource-table";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";
import { cn } from "@/lib/utils";

const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";
const COLUMNS = 5;

// A section with more rows than this draws only the rows in view; the ticks and the summary always
// cover every row (UI-SPEC "Long lists").
export const VIRTUALISE_OVER = 100;
const ROW_PX = 36;
const OVERSCAN = 10;
// How long the ticking has to stop before the summary line is announced.
export const ANNOUNCE_AFTER_MS = 500;

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
  items: JanitorItem[];
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
    out.push({ type, nodes, items: own });
  }
  return out;
}

type Row =
  | { kind: "node"; key: string; node: NodeGroupData }
  | { kind: "item"; key: string; item: JanitorItem };

// The rows of a section in the order they are drawn: on a swarm each node's header, then its items.
function rowsOf(group: TypeGroupData, swarm: boolean): Row[] {
  const rows: Row[] = [];
  for (const node of group.nodes) {
    if (swarm && node.nodeId) {
      rows.push({ kind: "node", key: `node|${group.type}|${node.nodeId}`, node });
    }
    for (const item of node.items) rows.push({ kind: "item", key: itemKey(item), item });
  }
  return rows;
}

type TriState = boolean | "indeterminate";

// How a group's checkbox stands: nothing, everything or part of it ticked. Part of it is
// "indeterminate", which the checkbox sets as aria-checked="mixed".
function triState(items: readonly JanitorItem[], ticked: ReadonlySet<string>): TriState {
  let n = 0;
  for (const item of items) if (ticked.has(itemKey(item))) n++;
  return n === 0 ? false : n === items.length ? true : "indeterminate";
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
  // and an item that was not there before starts ticked too. The set covers every item, not only
  // the rows that are drawn.
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
  // A group's checkbox ticks the whole group, or clears it when it was all ticked.
  function toggleGroup(group: readonly JanitorItem[]) {
    const next = new Set(ticked);
    if (group.every((i) => next.has(itemKey(i)))) for (const i of group) next.delete(itemKey(i));
    else for (const i of group) next.add(itemKey(i));
    setTicks({ of: data, set: next });
  }

  const line = useMemo(() => {
    const counts = { containers: 0, networks: 0, images: 0 };
    let bytes = 0;
    let known = false;
    for (const item of items) {
      if (!ticked.has(itemKey(item))) continue;
      counts[`${item.type}s` as keyof typeof counts]++;
      if (item.sizeBytes !== null) {
        bytes += item.sizeBytes;
        known = true;
      }
    }
    return { counts, estBytes: known ? bytes : null };
  }, [items, ticked]);

  const permissionReason = needs.allowed
    ? undefined
    : t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName });
  const removeReason =
    permissionReason ?? (ticked.size === 0 ? t("janitor.preview.footer.tickOne") : undefined);

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
          {data && items.length > 0 ? (
            <PreviewSummary
              key={data.previewedAt}
              line={summaryLine({ ...line.counts, estBytes: line.estBytes }, t)}
              sized={line.estBytes !== null && ticked.size > 0}
              note={
                <>
                  {t("janitor.preview.summary.note", { ticked: ticked.size, total: items.length })}{" "}
                  {t("janitor.preview.summary.previewed")} <RelativeTime value={data.previewedAt} />
                  .
                </>
              }
              refreshing={preview.isPending}
              onRefresh={() => preview.mutate()}
            />
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
              onToggleGroup={toggleGroup}
            />
          ))}
        </div>
        <PreviewFooter
          cancelRef={cancelRef}
          onCancel={() => onOpenChange(false)}
          commit={
            data && items.length > 0 ? (
              <NeedsTooltip reason={removeReason}>
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
            ) : null
          }
        />
      </SheetContent>
    </Sheet>
  );
}

// useSettled follows a value after it has stopped changing for a while: the screen reader hears the
// summary once, not for each of twenty ticks.
function useSettled(value: string, ms: number): string {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(id);
  }, [value, ms]);
  return settled;
}

interface PreviewSummaryProps {
  line: string;
  // True when the line ends in an estimated size, which the tooltip explains.
  sized: boolean;
  note: ReactNode;
  refreshing: boolean;
  onRefresh: () => void;
}

// PreviewSummary is the sheet's focal point: what would be removed, which follows the ticks. The line
// on the page changes at once; the live region repeats it once the ticking has settled. It is made
// afresh for each preview (its key), so a new preview is announced without waiting.
function PreviewSummary({ line, sized, note, refreshing, onRefresh }: PreviewSummaryProps) {
  const t = useT();
  const settled = useSettled(line, ANNOUNCE_AFTER_MS);
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        <p data-slot="preview-summary" className="text-base font-semibold">
          {line}
        </p>
        {sized ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span
                // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus
                tabIndex={0}
                role="img"
                aria-label={t("janitor.preview.summary.sizeTooltip")}
                className="rounded text-muted-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
              >
                <Info className="size-3.5" aria-hidden="true" />
              </span>
            </TooltipTrigger>
            <TooltipContent className="max-w-80">
              {t("janitor.preview.summary.sizeTooltip")}
            </TooltipContent>
          </Tooltip>
        ) : null}
      </div>
      <span
        data-slot="preview-summary-live"
        aria-live="polite"
        aria-atomic="true"
        className="sr-only"
      >
        {settled}
      </span>
      <p className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
        <span>{note}</span>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="h-6 px-2 text-xs max-md:h-11"
          disabled={refreshing}
          onClick={onRefresh}
        >
          {t("janitor.preview.summary.refresh")}
        </Button>
      </p>
    </div>
  );
}

interface PreviewFooterProps {
  cancelRef: RefObject<HTMLButtonElement | null>;
  onCancel: () => void;
  // The destructive commit, or nothing when there is nothing to remove.
  commit: ReactNode;
}

// The footer stays in view: what TierPlan does before it removes anything, Cancel (where focus
// starts) and the commit.
function PreviewFooter({ cancelRef, onCancel, commit }: PreviewFooterProps) {
  const t = useT();
  return (
    <SheetFooter className="sticky bottom-0 mt-0 flex-col gap-3 border-t bg-popover md:px-6">
      <p className="text-xs text-muted-foreground">{t("janitor.preview.footer.note")}</p>
      <div className="flex flex-col-reverse gap-2 sm:flex-row sm:justify-end">
        <Button
          ref={cancelRef}
          type="button"
          variant="outline"
          className="max-md:h-11"
          onClick={onCancel}
        >
          {t("janitor.preview.footer.cancel")}
        </Button>
        {commit}
      </div>
    </SheetFooter>
  );
}

interface TypeSectionProps {
  group: TypeGroupData;
  swarm: boolean;
  ticked: ReadonlySet<string>;
  onToggle: (key: string) => void;
  onToggleGroup: (items: readonly JanitorItem[]) => void;
}

// A type section is a collapsible card, open to start with: its title says how many there are and
// its checkbox ticks them all.
function TypeSection({ group, swarm, ticked, onToggle, onToggleGroup }: TypeSectionProps) {
  const t = useT();
  const rows = useMemo(() => rowsOf(group, swarm), [group, swarm]);
  // The scrolling element is state and not a ref: the rows below it ask for it before React has set a
  // ref on their parent, and a ref would still be empty then.
  const [scroller, setScroller] = useState<HTMLDivElement | null>(null);
  const virtual = rows.length > VIRTUALISE_OVER;
  const title = t(`janitor.preview.sections.${group.type}`, { count: group.items.length });

  const renderRow = (row: Row) =>
    row.kind === "node" ? (
      <NodeGroup
        key={row.key}
        node={row.node}
        state={triState(row.node.items, ticked)}
        onToggle={() => onToggleGroup(row.node.items)}
      />
    ) : (
      <PreviewRow
        key={row.key}
        item={row.item}
        ticked={ticked.has(row.key)}
        onToggle={() => onToggle(row.key)}
      />
    );

  return (
    <Collapsible defaultOpen asChild>
      <section aria-label={title} className="rounded-lg border">
        <div className="flex h-12 items-center gap-1 ps-3">
          <SelectCheckbox
            checked={triState(group.items, ticked)}
            label={t("janitor.preview.selectAll", {
              group: t(`janitor.preview.groups.${group.type}`),
            })}
            onToggle={() => onToggleGroup(group.items)}
          />
          <CollapsibleTrigger className="group flex h-full min-w-0 flex-1 items-center justify-between gap-2 px-3 text-start text-base font-semibold outline-none focus-visible:ring-3 focus-visible:ring-ring/50">
            <span>{title}</span>
            <ChevronDown
              aria-hidden="true"
              className="size-4 shrink-0 transition-transform group-data-[state=open]:rotate-180"
            />
          </CollapsibleTrigger>
        </div>
        <CollapsibleContent>
          <div
            ref={setScroller}
            className={cn("border-t", virtual ? "max-h-[60svh] overflow-auto" : "overflow-x-auto")}
          >
            <table className="w-full caption-bottom text-sm">
              <TableHeader>
                <TableRow>
                  <TableHead className="w-11" />
                  <TableHead className="min-w-48">{t("janitor.preview.columns.name")}</TableHead>
                  <TableHead className="min-w-64">{t("janitor.preview.columns.reason")}</TableHead>
                  <TableHead className="w-24">{t("janitor.preview.columns.age")}</TableHead>
                  <TableHead className="w-24 text-end">
                    {t("janitor.preview.columns.size")}
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {virtual ? (
                  <VirtualRows rows={rows} scroller={scroller} renderRow={renderRow} />
                ) : (
                  rows.map(renderRow)
                )}
              </TableBody>
            </table>
          </div>
        </CollapsibleContent>
      </section>
    </Collapsible>
  );
}

interface VirtualRowsProps {
  rows: readonly Row[];
  scroller: HTMLDivElement | null;
  renderRow: (row: Row) => ReactNode;
}

// VirtualRows draws the rows in view between two spacer rows that hold the height of the rest, so
// the table stays a native table and a long list stays responsive.
function VirtualRows({ rows, scroller, renderRow }: VirtualRowsProps) {
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroller,
    estimateSize: () => ROW_PX,
    overscan: OVERSCAN,
    getItemKey: (index) => rows[index]?.key ?? index,
  });
  const inView = virtualizer.getVirtualItems();
  const first = inView[0];
  const last = inView[inView.length - 1];
  const before = first?.start ?? 0;
  const after = last ? virtualizer.getTotalSize() - last.end : 0;
  return (
    <>
      {before > 0 ? (
        <tr style={{ height: before }}>
          <td colSpan={COLUMNS} />
        </tr>
      ) : null}
      {inView.map((v) => {
        const row = rows[v.index];
        return row ? renderRow(row) : null;
      })}
      {after > 0 ? (
        <tr style={{ height: after }}>
          <td colSpan={COLUMNS} />
        </tr>
      ) : null}
    </>
  );
}

interface NodeGroupProps {
  node: NodeGroupData;
  state: TriState;
  onToggle: () => void;
}

// A node's header row inside a type section: its name, how many items it holds and a checkbox for them.
function NodeGroup({ node, state, onToggle }: NodeGroupProps) {
  const t = useT();
  return (
    <TableRow className={cn(ROW, "bg-muted/50")}>
      <TableCell className={cn(CELL, "w-11")}>
        <SelectCheckbox
          checked={state}
          label={t("janitor.preview.selectAll", { group: node.node })}
          onToggle={onToggle}
        />
      </TableCell>
      <TableCell colSpan={COLUMNS - 1} className={CELL}>
        <span className="font-mono text-xs font-semibold">{node.node}</span>{" "}
        <span className="text-xs text-muted-foreground">
          {t("janitor.preview.nodeGroup.items", { count: node.items.length })}
        </span>
      </TableCell>
    </TableRow>
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
