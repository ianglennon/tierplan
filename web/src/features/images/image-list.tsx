// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Ellipsis, TriangleAlert } from "lucide-react";
import { type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";

import { useAllReadOnly } from "@/api/pulls";
import { type ImageRow, imagesQuery } from "@/api/resources";
import { NoPermission } from "@/components/full-page-state";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { NewItemsPill } from "@/components/new-items-pill";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Label } from "@/components/ui/label";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type BulkReport, BulkResultDialog } from "@/features/containers/bulk-result-dialog";
import { SearchBox } from "@/features/containers/container-list";
import { MAX_QUERY } from "@/features/containers/list-search";
import { PendingContext, usePendingRows } from "@/features/containers/pending";
import { NodesBanner } from "@/features/environments/nodes-banner";
import {
  DanglingDeleteDialog,
  type DanglingTarget,
} from "@/features/images/dangling-delete-dialog";
import { ImageNodesCell } from "@/features/images/image-nodes-cell";
import { inUseWhat } from "@/features/images/in-use";
import { RemoveImageDialog } from "@/features/images/remove-image-dialog";
import { UntagDialog } from "@/features/images/untag-dialog";
import { usedWhat } from "@/features/images/used-by";
import { PullDialog, PullImageButton } from "@/features/pulls/pull-dialog";
import { BulkBar } from "@/features/resources/bulk-bar";
import { useFrozenRows } from "@/features/resources/frozen-rows";
import {
  type Column,
  NodeSelect,
  ResourceTable,
  type RowState,
  SelectCheckbox,
  type TableSelect,
  TruncatedText,
} from "@/features/resources/resource-table";
import { useChosenKeys, useSelection } from "@/features/resources/selection";
import { STATUS_CHIP_ROW } from "@/features/resources/status-chips";
import {
  nodeRowKey,
  nodeSearchSchema,
  useEnvIsSwarm,
  useNodeColumn,
} from "@/features/resources/swarm-lists";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";
import { toastError } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// The images route's search (UI-SPEC "Screens"): the node, the search text, the Dangling chip
// and how a swarm's rows are grouped. A router reads `dangling=1` as the number 1, so both it
// and the text are taken; every value that does not fit is dropped, not sent.
export const imageSearchSchema = nodeSearchSchema.extend({
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  dangling: z
    .union([z.literal(1), z.literal("1")])
    .transform(() => 1 as const)
    .optional()
    .catch(undefined),
  view: z
    .enum(["merged", "node"])
    .transform((v) => (v === "node" ? ("node" as const) : undefined))
    .optional()
    .catch(undefined),
});

export type ImageSearch = z.output<typeof imageSearchSchema>;

// A change to the search: a key set to undefined leaves the URL.
export type ImageSearchPatch = { [K in keyof ImageSearch]?: ImageSearch[K] | undefined };

// The engine reports an untagged image as "<none>" or "<none>:<none>".
const isNone = (tag: string) => tag === "<none>" || tag === "<none>:<none>";

// The key of a row in the list: a merged swarm row has no node of its own.
const rowKeyOf = (i: ImageRow) => nodeRowKey(i.nodeId, i.id);

// What the pending mark compares: it differs once the list shows the row changed (a node lost
// the image, or something started using it).
const pendingFingerprint = (i: ImageRow) =>
  `${(i.nodes ?? []).map((n) => n.nodeId).join(",")}|${i.usedByContainers}|${i.usedByServices}`;

// What the frozen list compares to see a row changed in place.
const fingerprintOf = (i: ImageRow) =>
  [
    (i.repoTags ?? []).join(","),
    i.size,
    i.usedByContainers,
    i.usedByServices,
    (i.nodes ?? []).map((n) => `${n.nodeId}:${n.containers}`).join(","),
  ].join("|");

// Only a dangling image nothing uses can go in a bulk delete (D-17).
const inUse = (i: ImageRow) => i.usedByContainers + i.usedByServices > 0;
const selectable = (i: ImageRow) => i.dangling && !inUse(i);

// checkedOf is a checkbox's state from a selection's.
function checkedOf(state: "checked" | "mixed" | "unchecked"): boolean | "indeterminate" {
  return state === "checked" ? true : state === "mixed" ? "indeterminate" : false;
}

// ReadOnlyAlert is the warning above the table when the owner of every node's host turned
// writes off: images can be looked at, not pulled or removed (D-30).
function ReadOnlyAlert({ envId }: { envId: string }) {
  const t = useT();
  if (!useAllReadOnly(envId)) return null;
  return (
    <Alert variant="warning">
      <TriangleAlert aria-hidden="true" />
      <AlertDescription className="text-foreground">{t("pulls.readOnly.alert")}</AlertDescription>
    </Alert>
  );
}

// ImageCell is the first tag in mono, then "+n" with a tooltip listing the rest; an untagged
// image is Untagged in muted with the outline badge Dangling. The whole cell opens the image's
// page, which reads every node that has it.
function ImageCell({ envId, item, perNode }: { envId: string; item: ImageRow; perNode: boolean }) {
  const t = useT();
  const tags = (item.repoTags ?? []).filter((tag) => !isNone(tag));
  const [first, ...rest] = tags;
  return (
    <span className="flex min-w-0 items-center gap-2">
      <Link
        to="/env/$envId/images/$imageId"
        params={{ envId, imageId: item.id }}
        search={{ node: perNode ? item.nodeId || undefined : undefined }}
        className="block min-w-0 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
      >
        {first ? (
          <TruncatedText mono className="max-w-80">
            {first}
          </TruncatedText>
        ) : (
          <span className="text-muted-foreground">{t("images.list.untagged")}</span>
        )}
      </Link>
      {first === undefined ? (
        <Badge variant="outline" className="shrink-0">
          {t("images.list.dangling.badge")}
        </Badge>
      ) : null}
      {rest.length > 0 ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <button
              type="button"
              className="shrink-0 cursor-help rounded-sm text-xs text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
              aria-label={t("images.list.moreNames", { names: rest.join(", ") })}
            >
              {t("images.list.more", { count: rest.length })}
            </button>
          </TooltipTrigger>
          <TooltipContent className="flex flex-col gap-0.5 font-mono text-xs break-all">
            {rest.map((name) => (
              <span key={name}>{name}</span>
            ))}
          </TooltipContent>
        </Tooltip>
      ) : null}
    </span>
  );
}

// ImageRowActions is the row's menu: Open, Pull again…, Remove… and, when the image has two or
// more names, Untag…. Remove… is unavailable, with the reason, for a caller who may not, on a
// read-only host, and while something uses the image on every node that has it; Untag… is
// allowed while the image is in use, because the image stays under its other names (D-28,
// D-29, D-30).
function ImageRowActions({
  envId,
  item,
  swarm,
  perNode,
  hostName,
  disabled,
  onOpenChange,
}: {
  envId: string;
  item: ImageRow;
  swarm: boolean;
  perNode: boolean;
  hostName: string;
  // The row is waiting on a removal: its menu cannot be opened.
  disabled?: boolean;
  // Told whether a menu or a dialog of the row is open, so the list holds still under it.
  onOpenChange?: (open: boolean) => void;
}) {
  const t = useT();
  const remove = useNeeds("images.delete", envId);
  const pull = useNeeds("images.pull", envId);
  const tags = (item.repoTags ?? []).filter((tag) => !isNone(tag));
  const short = item.shortId || item.id.replace(/^sha256:/, "").slice(0, 12);
  const name = tags[0] ?? short;
  const holders = item.nodes ?? [];
  const blockedEverywhere =
    item.usedByServices > 0 || (holders.length > 0 && holders.every((n) => n.containers > 0));
  const changeReason = remove.reason ?? (item.readOnly ? t("pulls.readOnly.reason") : undefined);
  const removeReason =
    changeReason ??
    (blockedEverywhere
      ? t("images.remove.inUseReason", {
          what: inUseWhat(t, item.usedByContainers, item.usedByServices),
        })
      : undefined);
  const pullReason = pull.reason ?? (item.readOnly ? t("pulls.readOnly.reason") : undefined);
  const [menuOpen, setMenuOpen] = useState(false);
  const [dialog, setDialog] = useState<"pull" | "remove" | "untag" | null>(null);
  // Set when an item was chosen, so the menu does not hand focus back to its button before the
  // dialog has opened.
  const chose = useRef(false);
  const canUntag = tags.length > 1;
  const reportOpen = useRef(onOpenChange);
  reportOpen.current = onOpenChange;
  const open = menuOpen || dialog !== null;
  useEffect(() => {
    reportOpen.current?.(open);
    return () => reportOpen.current?.(false);
  }, [open]);

  function choose(which: "pull" | "remove" | "untag") {
    chose.current = true;
    setDialog(which);
  }

  const item_ = (label: string, onSelect: () => void, destructive = false) => (
    <DropdownMenuItem
      className="max-md:min-h-11"
      variant={destructive ? "destructive" : "default"}
      onSelect={onSelect}
    >
      {label}
    </DropdownMenuItem>
  );

  return (
    <>
      <DropdownMenu open={menuOpen} onOpenChange={setMenuOpen}>
        <DropdownMenuTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            disabled={disabled}
            aria-label={t("actions.menu.label", { name })}
          >
            <Ellipsis aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="end"
          onCloseAutoFocus={(event) => {
            if (!chose.current) return;
            chose.current = false;
            event.preventDefault();
          }}
        >
          <DropdownMenuItem asChild className="max-md:min-h-11">
            <Link
              to="/env/$envId/images/$imageId"
              params={{ envId, imageId: item.id }}
              search={{ node: perNode ? item.nodeId || undefined : undefined }}
            >
              {t("actions.menu.open")}
            </Link>
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          {tags.length > 0 ? (
            <NeedsTooltip reason={pullReason}>
              {item_(t("images.menu.pull"), () => choose("pull"))}
            </NeedsTooltip>
          ) : null}
          <NeedsTooltip reason={removeReason}>
            {item_(t("images.remove.action"), () => choose("remove"), true)}
          </NeedsTooltip>
          {canUntag ? (
            <NeedsTooltip reason={changeReason}>
              {item_(t("images.untag.action"), () => choose("untag"))}
            </NeedsTooltip>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      {remove.allowed ? (
        <RemoveImageDialog
          envId={envId}
          imageId={item.id}
          name={name}
          nodes={holders.map((n) => ({ nodeId: n.nodeId, name: n.name }))}
          swarm={swarm}
          open={dialog === "remove"}
          onClose={() => setDialog(null)}
        />
      ) : null}
      {remove.allowed && canUntag ? (
        <UntagDialog
          envId={envId}
          imageId={item.id}
          names={tags}
          // The row knows the names of the image, not which node has which; a node without a
          // name answers not found for it.
          nodes={holders.map((n) => ({ nodeId: n.nodeId, name: n.name, tags }))}
          swarm={swarm}
          open={dialog === "untag"}
          onClose={() => setDialog(null)}
        />
      ) : null}
      {pull.allowed && tags.length > 0 ? (
        <PullDialog
          envId={envId}
          open={dialog === "pull"}
          onOpenChange={(open) => setDialog(open ? "pull" : null)}
          initialRef={name}
          hostName={hostName}
          swarm={swarm}
        />
      ) : null}
    </>
  );
}

interface ImageListProps {
  envId: string;
  // The route's validated search, and the way to change it: a key set to undefined leaves the
  // URL. Changes replace the history entry.
  search: ImageSearch;
  onSearchChange: (patch: ImageSearchPatch) => void;
}

// ImageList is the images list of an environment. On a swarm it merges the node copies of an
// image by its ID, with a Nodes cell that says where it is, or lists one row per node copy
// (Group by node); the search, the Dangling chip and the node filter run on the server before
// its 500-row cap, and each row says what uses the image (D-16, D-17, RES-09).
export function ImageList({ envId, search, onSearchChange }: ImageListProps) {
  const t = useT();
  const key = "dashboard.lists.images";
  const viewId = useId();
  const allowed = usePermission("images.read", envId);
  const swarm = useEnvIsSwarm(envId);
  const nodeColumn = useNodeColumn<ImageRow>();
  const node = swarm ? search.node : undefined;
  const perNode = swarm && search.view === "node";
  const dangling = search.dangling !== undefined;
  const query = useQuery({
    ...imagesQuery(envId, {
      node,
      q: search.q,
      dangling,
      view: perNode ? "node" : "merged",
    }),
    enabled: allowed,
  });
  const data = query.data;
  const envName = useEnvName(envId);
  const missing = t("dashboard.time.missing");
  const filtered = Boolean(search.q) || dangling || node !== undefined;
  const clearFilters = () => onSearchChange({ q: undefined, dangling: undefined, node: undefined });
  const queryClient = useQueryClient();
  const view = perNode ? "node" : "merged";
  const remove = useNeeds("images.delete", envId);

  // Rows with a menu or a dialog open hold the list still, and so do rows chosen for a bulk
  // delete (D-05): a row that moved between choosing it and pressing the button would be the
  // wrong one. Changed rows still update in place; new ones wait behind the pill.
  const [openRows, setOpenRows] = useState<ReadonlySet<string>>(() => new Set());
  const setRowOpen = (key: string, isOpen: boolean) =>
    setOpenRows((prev) => {
      if (prev.has(key) === isOpen) return prev;
      const next = new Set(prev);
      if (isOpen) next.add(key);
      else next.delete(key);
      return next;
    });
  const chosen = useChosenKeys<string>();
  const sorted = useMemo(() => data?.items ?? [], [data]);
  const resetKey = [envId, node, search.q, dangling, view].join("\u0000");
  const frozen = useFrozenRows(sorted, {
    frozen: openRows.size > 0 || chosen[0].size > 0,
    getKey: rowKeyOf,
    fingerprint: fingerprintOf,
    // Rows of the previous filters, kept on screen while the new ones load, are not an
    // earlier answer to this question.
    ready: data !== undefined && !query.isPlaceholderData,
    resetKey,
  });
  const shownRows = useMemo(() => frozen.rows.map((r) => r.row), [frozen.rows]);
  const rowStates = useMemo(() => {
    const out = new Map<string, RowState>();
    for (const r of frozen.rows) {
      if (r.changed || r.added || r.removed) {
        out.set(rowKeyOf(r.row), {
          changed: r.changed,
          added: r.added,
          removed: r.removed,
          leaving: r.leaving,
        });
      }
    }
    return out;
  }, [frozen.rows]);
  const {
    pending,
    api: pendingApi,
    announcement,
  } = usePendingRows(shownRows, { getKey: rowKeyOf, fingerprint: pendingFingerprint });

  // The dangling images of the view that the list does not show, read when "Select all" is
  // asked for (the list is capped at 500 rows, and the Dangling chip may be off).
  const [extra, setExtra] = useState<ImageRow[]>([]);
  const [loadedAll, setLoadedAll] = useState(false);
  const [loadingAll, setLoadingAll] = useState(false);
  const wantEvery = useRef(false);
  useEffect(() => {
    // A different list: what was read for the last one is no longer its.
    void resetKey;
    setExtra([]);
    setLoadedAll(false);
  }, [resetKey]);

  // Which rows can be chosen: dangling and unused, not gone, not waiting on a removal. The
  // order a Shift range follows is the order on screen.
  const byKey = useMemo(() => {
    const out = new Map(extra.map((r) => [rowKeyOf(r), r]));
    for (const r of shownRows) out.set(rowKeyOf(r), r);
    return out;
  }, [shownRows, extra]);
  const orderedKeys = useMemo(() => shownRows.map(rowKeyOf), [shownRows]);
  const selection = useSelection(
    {
      orderedKeys,
      isEligible: (key) => {
        const r = byKey.get(key);
        return (
          r !== undefined && selectable(r) && !rowStates.get(key)?.removed && !pending.has(key)
        );
      },
    },
    chosen,
  );
  const selectionRef = useRef(selection);
  selectionRef.current = selection;
  // The rows read for "Select all" are chosen once they are known to the selection.
  useEffect(() => {
    if (!wantEvery.current || extra.length === 0) return;
    wantEvery.current = false;
    selectionRef.current.selectEvery(extra.map(rowKeyOf));
  }, [extra]);

  const [dialog, setDialog] = useState<{ rows: DanglingTarget[]; open: boolean }>({
    rows: [],
    open: false,
  });
  const [report, setReport] = useState<BulkReport | null>(null);
  const clearSelection = () => {
    selection.clear();
    setExtra([]);
    setLoadedAll(false);
  };
  const selectedRows = [...byKey.values()].filter((r) => selection.isSelected(rowKeyOf(r)));
  function startDelete() {
    setDialog({
      open: true,
      rows: selectedRows.map((r) => ({
        rowKey: rowKeyOf(r),
        imageId: r.id,
        shortId: r.shortId || r.id.replace(/^sha256:/, "").slice(0, 12),
        fingerprint: pendingFingerprint(r),
        nodes:
          (r.nodes ?? []).length > 0
            ? (r.nodes ?? []).map((n) => ({
                nodeId: n.nodeId,
                name: n.name,
                inUse: n.containers > 0,
              }))
            : [{ nodeId: r.nodeId, name: r.node }],
      })),
    });
  }
  async function selectEvery() {
    setLoadingAll(true);
    try {
      const all = await queryClient.fetchQuery({
        ...imagesQuery(envId, { node, dangling: true, view }),
        staleTime: 0,
      });
      wantEvery.current = true;
      setExtra(all.items ?? []);
      setLoadedAll(true);
    } catch {
      toastError(t("images.bulk.selectEvery"));
    } finally {
      setLoadingAll(false);
    }
  }
  const changesWaiting = frozen.heldCount + frozen.removedCount;

  const nodesColumn: Column<ImageRow> | null = !swarm
    ? null
    : perNode
      ? nodeColumn
      : {
          id: "nodes",
          header: t("images.list.columns.nodes"),
          className: "w-36 min-w-36",
          cell: (i) => (
            <ImageNodesCell
              item={i}
              nodeRefs={data?.nodeRefs ?? []}
              nodeCount={data?.nodeCount ?? 0}
              notReporting={data?.notReporting}
            />
          ),
        };

  const columns: Column<ImageRow>[] = [
    {
      id: "image",
      header: t("images.list.columns.image"),
      className: "min-w-[240px]",
      cell: (i) => <ImageCell envId={envId} item={i} perNode={perNode} />,
    },
    {
      id: "id",
      header: t(`${key}.columns.id`),
      className: "w-32 min-w-32",
      cell: (i) => {
        const short = i.shortId || i.id.replace(/^sha256:/, "").slice(0, 12);
        return short ? <span className="font-mono text-xs">{short}</span> : missing;
      },
    },
    ...(nodesColumn ? [nodesColumn] : []),
    {
      id: "usedBy",
      header: t("images.list.columns.usedBy"),
      className: "w-48 min-w-48",
      cell: (i) =>
        pending.has(rowKeyOf(i)) ? (
          <span className="inline-flex items-center whitespace-nowrap" data-pending="remove">
            <StatusDot state="reconnecting" label={t("actions.pending.remove")} />
          </span>
        ) : i.usedByContainers + i.usedByServices > 0 ? (
          usedWhat(t, i.usedByContainers, i.usedByServices, swarm)
        ) : (
          <span className="text-muted-foreground">{t("images.notInUse")}</span>
        ),
    },
    {
      id: "size",
      header: t(`${key}.columns.size`),
      className: "w-28 min-w-28 text-right",
      cell: (i) => <span className="tabular-nums">{i.size ? formatBytes(i.size) : missing}</span>,
    },
    {
      id: "created",
      header: t(`${key}.columns.created`),
      className: "w-[120px]",
      cell: (i) => <RelativeTime value={i.created} />,
    },
    {
      id: "actions",
      header: <span className="sr-only">{t("actions.column")}</span>,
      className: "w-11 px-1 text-end",
      cell: (i) => (
        <ImageRowActions
          envId={envId}
          item={i}
          swarm={swarm}
          perNode={perNode}
          hostName={envName}
          disabled={pending.has(rowKeyOf(i)) || Boolean(rowStates.get(rowKeyOf(i))?.removed)}
          onOpenChange={(isOpen) => setRowOpen(rowKeyOf(i), isOpen)}
        />
      ),
    },
  ];

  // The caption names the view: images, or on a swarm images across the nodes, or the copies
  // across the nodes; with a search or filter, how many of how many.
  const nodeTotal = data?.nodeCount ?? 0;
  let caption: string | undefined;
  if (data) {
    if (filtered) {
      caption = t("images.list.caption.filtered", {
        count: data.total,
        total: data.unfilteredTotal,
      });
    } else if (swarm && nodeTotal > 0) {
      caption = t(perNode ? "images.list.caption.perNode" : "images.list.caption.merged", {
        what: t(perNode ? "images.list.copies" : `${key}.count`, { count: data.total }),
        nodes: t("aggregate.caption.across", { count: nodeTotal }),
      });
    }
  }

  const pull = <PullImageButton envId={envId} hostName={envName} swarm={swarm} />;
  // The four empty states: none at all, the Dangling chip on, a search, and a node with none.
  let empty: ReactNode;
  if (search.q) {
    empty = (
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {t("images.list.empty.search", { query: search.q })}
          </EmptyTitle>
        </EmptyHeader>
        <EmptyContent>
          <Button
            variant="outline"
            className="max-md:h-11"
            onClick={() => onSearchChange({ q: undefined })}
          >
            {t("images.list.empty.clearSearch")}
          </Button>
        </EmptyContent>
      </Empty>
    );
  } else if (dangling) {
    empty = (
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {t("images.list.empty.dangling.title")}
          </EmptyTitle>
          <EmptyDescription>{t("images.list.empty.dangling.body")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    );
  } else if (node !== undefined) {
    empty = (
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {t("images.list.empty.node", { node })}
          </EmptyTitle>
        </EmptyHeader>
        <EmptyContent>
          <Button variant="outline" className="max-md:h-11" onClick={clearFilters}>
            {t("images.list.clearFilters")}
          </Button>
        </EmptyContent>
      </Empty>
    );
  } else {
    empty = (
      <Empty className="min-h-[30svh] border">
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={2} className="text-base">
            {t(`${key}.empty`)}
          </EmptyTitle>
          <EmptyDescription>{t("dashboard.lists.emptyBody", { name: envName })}</EmptyDescription>
        </EmptyHeader>
        <EmptyContent>{pull}</EmptyContent>
      </Empty>
    );
  }
  const noneAtAll = !filtered && data !== undefined && data.total === 0;

  const toolbar = (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center gap-2 max-md:[&>*]:min-w-0">
        <SearchBox
          value={search.q}
          onCommit={(q) => onSearchChange({ q })}
          label={t("images.list.search.label")}
          placeholder={t("images.list.search.placeholder")}
          clearLabel={t("images.list.search.clear")}
        />
        {swarm ? (
          <NodeSelect
            value={node}
            onChange={(n) => onSearchChange({ node: n })}
            nodes={data?.nodes ?? []}
          />
        ) : null}
        {filtered ? (
          <Button variant="ghost" className="max-md:h-11" onClick={clearFilters}>
            {t("images.list.clearFilters")}
          </Button>
        ) : null}
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <ToggleGroup
          type="multiple"
          // The chips are toggle buttons in a labelled group, not a toolbar.
          role="group"
          variant="outline"
          size="sm"
          spacing={2}
          aria-label={t("images.list.filter.label")}
          value={dangling ? ["dangling"] : []}
          onValueChange={(v) =>
            onSearchChange({ dangling: v.includes("dangling") ? 1 : undefined })
          }
          className={STATUS_CHIP_ROW}
        >
          <ToggleGroupItem
            value="dangling"
            className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
          >
            {data
              ? t("images.list.filter.danglingCount", { count: data.danglingCount })
              : t("images.list.filter.dangling")}
          </ToggleGroupItem>
        </ToggleGroup>
        {swarm ? (
          <div className="flex items-center gap-2">
            <Label id={viewId} className="text-sm text-muted-foreground">
              {t("images.list.view.label")}
            </Label>
            <ToggleGroup
              type="single"
              role="group"
              variant="outline"
              size="sm"
              spacing={2}
              aria-labelledby={viewId}
              value={perNode ? "node" : "merged"}
              onValueChange={(v) => {
                // Choosing the chosen one again is not a choice.
                if (v) onSearchChange({ view: v === "node" ? "node" : undefined });
              }}
            >
              {(["merged", "node"] as const).map((v) => (
                <ToggleGroupItem
                  key={v}
                  value={v}
                  className="aria-checked:border-ring aria-checked:text-ring max-md:h-11"
                >
                  {t(`images.list.view.${v}`)}
                </ToggleGroupItem>
              ))}
            </ToggleGroup>
          </div>
        ) : null}
      </div>
    </div>
  );

  // The table shows the rows as the frozen list holds them.
  const tableQuery = useMemo(
    () => ({ ...query, data: data ? { ...data, items: shownRows } : undefined }),
    [query, data, shownRows],
  );

  // The checkbox column. A row that cannot go in a bulk keeps a checkbox that cannot be ticked
  // and says why (D-17, D-30).
  const select: TableSelect<ImageRow> = {
    header: (
      <SelectCheckbox
        checked={checkedOf(selection.allState)}
        label={t("images.bulk.selectAll")}
        disabled={selection.eligibleCount === 0}
        onToggle={() => selection.selectAll()}
      />
    ),
    cell: (i) => {
      const key = rowKeyOf(i);
      const short = i.shortId || i.id.replace(/^sha256:/, "").slice(0, 12);
      const waiting = pending.has(key) || Boolean(rowStates.get(key)?.removed);
      return (
        <SelectCheckbox
          checked={selection.isSelected(key)}
          label={t("actions.select.row", { name: (i.repoTags ?? [])[0] ?? short })}
          disabled={waiting}
          reason={
            waiting || selectable(i)
              ? undefined
              : i.dangling
                ? t("images.bulk.inUse")
                : t("images.bulk.notDangling")
          }
          onToggle={(shift) => selection.toggle(key, { shift })}
        />
      );
    },
    onEscape: selection.selected.size > 0 ? clearSelection : undefined,
  };

  const readOnlyAll = selectedRows.length > 0 && selectedRows.every((r) => r.readOnly);
  const bulkReason = remove.reason ?? (readOnlyAll ? t("pulls.readOnly.reason") : undefined);
  const moreExist =
    data !== undefined &&
    !loadedAll &&
    selection.allState === "checked" &&
    selection.selected.size > 0 &&
    data.danglingCount > selection.selected.size;

  if (!allowed) return <NoPermission />;
  return (
    <PendingContext.Provider value={pendingApi}>
      <div className="sr-only" role="status">
        {announcement
          ? t("actions.announce.remove", { names: announcement.names.join(", ") })
          : null}
      </div>
      {remove.allowed ? (
        <DanglingDeleteDialog
          envId={envId}
          rows={dialog.rows}
          swarm={swarm}
          open={dialog.open}
          onClose={() => setDialog((d) => ({ ...d, open: false }))}
          onDone={clearSelection}
          onDetails={setReport}
        />
      ) : null}
      <BulkResultDialog envId={envId} report={report} onClose={() => setReport(null)} />
      <ResourceTable
        resource="images"
        columns={columns}
        query={tableQuery}
        rowStates={rowStates}
        select={select}
        bulkBar={
          selection.selected.size > 0 ? (
            <BulkBar
              count={selection.selected.size}
              actions={[
                {
                  id: "remove",
                  label: t("images.bulk.remove"),
                  destructive: true,
                  reason: bulkReason,
                  onClick: bulkReason ? () => {} : startDelete,
                },
              ]}
              onClear={clearSelection}
              selectAll={
                moreExist && !loadingAll
                  ? { count: data.danglingCount, onSelect: () => void selectEvery() }
                  : undefined
              }
            />
          ) : undefined
        }
        envName={envName}
        toolbar={toolbar}
        empty={empty}
        caption={caption}
        banner={
          <>
            {swarm ? <NodesBanner notReporting={data?.notReporting} /> : null}
            <ReadOnlyAlert envId={envId} />
          </>
        }
        getRowKey={(i) => nodeRowKey(i.nodeId, i.id)}
        captionExtra={
          <>
            <NewItemsPill
              count={changesWaiting}
              label={t("images.bulk.changes", { count: changesWaiting })}
              action={t("images.bulk.showChanges")}
              onClick={frozen.release}
            />
            {noneAtAll ? null : (
              <div className="ml-auto flex flex-wrap items-center gap-2">{pull}</div>
            )}
          </>
        }
      />
    </PendingContext.Provider>
  );
}
