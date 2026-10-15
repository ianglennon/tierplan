// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useVirtualizer } from "@tanstack/react-virtual";
import {
  type KeyboardEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";

import { Button } from "@/components/ui/button";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import { Skeleton } from "@/components/ui/skeleton";
import type { Theme } from "@/features/logs/ansi";
import { DownloadDialog, DownloadPopover } from "@/features/logs/download-popover";
import {
  type BufferedLine,
  clockTime,
  LogBuffer,
  type LogLineData,
  stampDay,
} from "@/features/logs/log-buffer";
import { DayDividerRow, LINE_HEIGHT_PX, LogLineRow } from "@/features/logs/log-line";
import {
  formatShow,
  type LogViewParams,
  loadLogPrefs,
  loadTaskNames,
  parseShow,
  type ShowRange,
  saveLogPrefs,
  saveTaskNames,
  streamRange,
} from "@/features/logs/log-prefs";
import { isEndedTaskState, isSlotMark, type ServiceLogTask } from "@/features/logs/log-source";
import {
  createLogStream,
  fetchLogPage,
  type LogStream,
  type LogStreamState,
  makeViewerId,
  plusNanosecond,
  type StreamTarget,
  sourceOf,
  type UnreadableInfo,
} from "@/features/logs/log-stream";
import { counterText, LogToolbar } from "@/features/logs/log-toolbar";
import { swarmMarkerText } from "@/features/logs/replacement-marker";
import { searchLines } from "@/features/logs/search";
import {
  type FilterOptions,
  NO_FILTER_OPTIONS,
  TaskFilterBar,
} from "@/features/logs/task-filter-bar";
import {
  formatTaskPrefix,
  prefixColour,
  prefixWidthCh,
  TaskPrefix,
} from "@/features/logs/task-prefix";
import { UnreadableNotice } from "@/features/logs/unreadable-notice";
import { useT } from "@/i18n/use-t";
import { withBase } from "@/lib/base-path";
import { fitToViewport } from "@/lib/fit-to-viewport";

// Rows drawn beyond the visible ones, so a fast scroll does not show a blank.
const OVERSCAN = 20;
// How far from the end still counts as being at the end, in pixels. Scrolling up by
// any real amount leaves it (D-21); this is only the rounding of a browser's scroll.
const FOLLOW_SLACK_PX = 8;
// How long the search waits for typing to stop before it looks, and before it says the
// count aloud and writes the query into the address.
const SEARCH_DEBOUNCE_MS = 150;
const ANNOUNCE_DEBOUNCE_MS = 500;
// How many earlier lines one click of Load earlier lines brings.
const EARLIER_PAGE = 500;
// The widths of the skeleton lines shown until the stream connects.
const SKELETON_WIDTHS = ["w-3/5", "w-4/5", "w-2/5", "w-3/4", "w-1/2", "w-2/3", "w-4/5", "w-1/3"];

// A list from the address: comma separated, empty parts dropped.
const listOf = (v: string | undefined): string[] =>
  v
    ? v
        .split(",")
        .map((x) => x.trim())
        .filter(Boolean)
    : [];

// A task named in the address is its ID, or the start of one of at least eight characters.
const idMatches = (chosen: string, id: string): boolean =>
  chosen === id || (chosen.length >= 8 && id.startsWith(chosen));

// shownUnderFilters says whether a row of the buffer belongs to the tasks and nodes a log is
// narrowed to. It narrows the lines already loaded while the stream for the new set opens; the
// server narrows what follows. A row that names no task (a container's marker, the interrupted
// notice) stays. A replacement marker belongs to the old task and the new one, a node notice to
// its node, and says nothing about a task that is chosen.
function shownUnderFilters(
  line: BufferedLine,
  tasks: readonly string[],
  nodes: readonly string[],
): boolean {
  if (tasks.length === 0 && nodes.length === 0) return true;
  const mark = line.swarm;
  if (mark) {
    if (!isSlotMark(mark)) return tasks.length === 0 && nodes.includes(mark.node);
    const newId = mark.new;
    const taskOk =
      tasks.length === 0 ||
      tasks.some((c) => idMatches(c, mark.old) || (newId !== undefined && idMatches(c, newId)));
    const nodeOk =
      nodes.length === 0 ||
      nodes.includes(mark.oldNode) ||
      (mark.node !== undefined && nodes.includes(mark.node));
    return taskOk && nodeOk;
  }
  if (line.task === undefined) return true;
  const id = line.task;
  return (
    (tasks.length === 0 || tasks.some((c) => idMatches(c, id))) &&
    (nodes.length === 0 || nodes.includes(line.node ?? ""))
  );
}

export interface LogViewerProps extends StreamTarget {
  envId: string;
  // The container's or service's name, for the accessible name of the surface. For a
  // service it is also the name its task prefixes carry.
  name: string;
  // True where the service's own header is not on screen (the pop-out), so each prefix
  // names the service as well as the slot.
  showServiceName?: boolean;
  // The range, search and toggles from the address (D-22). With onParamsChange the
  // address owns them; without it the viewer keeps them itself.
  params?: LogViewParams;
  onParamsChange?: (params: LogViewParams) => void;
  // What the Tasks and Nodes filters of a service's log offer (plan 06-30, D-33).
  filterOptions?: FilterOptions;
  // Controls after Download… at the right of the toolbar's second row, and the items of
  // the More menu that stand in for them on a narrow screen.
  actions?: ReactNode;
  moreItems?: ReactNode;
  // This is the pop-out window: it does not offer another pop-out, and it closes its
  // stream when it has been hidden for a minute and opens it again when seen (D-22).
  popout?: boolean;
}

// How long a hidden pop-out keeps its stream before it lets it go.
export const HIDDEN_POPOUT_MS = 60_000;

// popoutLogsUrl is the address of the logs pop-out, with the range, the search and the
// toggles carried in the query the way the route reads them. A text that would read as
// a number, a boolean or JSON is quoted, since the router parses every value as JSON
// first; the address is under the base path.
export function popoutLogsUrl(
  envId: string,
  containerId: string,
  nodeId: string | undefined,
  view: LogViewParams,
): string {
  const text = (v: string): string => {
    try {
      JSON.parse(v);
      return JSON.stringify(v);
    } catch {
      return v;
    }
  };
  const q = new URLSearchParams();
  if (nodeId) q.set("node", text(nodeId));
  if (view.since) q.set("since", text(view.since));
  if (view.q) q.set("q", text(view.q));
  if (view.ts !== undefined) q.set("ts", String(view.ts));
  if (view.utc !== undefined) q.set("utc", String(view.utc));
  if (view.wrap !== undefined) q.set("wrap", String(view.wrap));
  const query = q.toString();
  return withBase(
    `/popout/${encodeURIComponent(envId)}/containers/${encodeURIComponent(containerId)}/logs${query ? `?${query}` : ""}`,
  );
}

// The theme the page is in: the dark class on the root.
function readTheme(): Theme {
  return document.documentElement.classList.contains("dark") ? "dark" : "light";
}

// useTheme follows the page's theme as it changes.
function useTheme(): Theme {
  const [theme, setTheme] = useState<Theme>(readTheme);
  useEffect(() => {
    const watch = new MutationObserver(() => setTheme(readTheme()));
    watch.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
    return () => watch.disconnect();
  }, []);
  return theme;
}

function useDebounced<T>(value: T, ms: number): T {
  const [v, setV] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setV(value), ms);
    return () => clearTimeout(id);
  }, [value, ms]);
  return v;
}

// LogViewer shows a container's log live: it opens on the last 500 lines and follows,
// drawing only the rows in view (a virtualised list of 20px rows). Scrolling away from
// the end stops the following until the end is reached again, and the lines that come
// meanwhile are counted on a pill. The lines live in a buffer of at most 50 000. Search
// works on what is loaded and never asks the server.
export function LogViewer(props: LogViewerProps) {
  const {
    envId,
    name,
    showServiceName = false,
    params,
    onParamsChange,
    filterOptions = NO_FILTER_OPTIONS,
    actions,
    moreItems,
    popout = false,
  } = props;
  const src = sourceOf(props);
  // A service's log shares everything with a container's but what the stream can serve
  // (download, earlier lines, pop-out are the container's for now) and what it adds: the
  // task of every line (plan 06-29, D-35).
  const kind = src.kind;
  const isService = kind === "service";
  const sourceId = src.id;
  const containerNode = src.kind === "container" ? src.nodeId : undefined;
  const t = useT();
  // The viewer's own ID: made once, sent on every stream and page of this viewer, so
  // the server audits it once however often the stream reconnects (D-24).
  const [sid] = useState(makeViewerId);
  const [buffer] = useState(() => new LogBuffer());
  const [state, setState] = useState<LogStreamState>({ status: "connecting" });
  // Whether the stream has been live: a failure before that is a failed load, with
  // Retry; one after it is a drop, which the stream itself retries.
  const everLive = useRef(false);
  // Bumped by Retry to open the stream again for the same viewer.
  const [openCount, setOpenCount] = useState(0);
  const streamRef = useRef<LogStream | null>(null);
  // Set when the engine cannot read this container's log back (D-25).
  const [unreadable, setUnreadable] = useState<UnreadableInfo | null>(null);
  // A service's tasks as the stream last listed them, null before it has, and the longest
  // task prefix loaded, which sets the width of the prefix gutter.
  const [tasks, setTasks] = useState<ServiceLogTask[] | null>(null);
  // The slot and node pairs seen so far: few however many lines, and what the width of the
  // prefix gutter is worked out from. prefixSeen counts them, to draw again when one is new.
  const prefixPairs = useRef(new Map<string, { slot: number; node: string }>());
  const [prefixSeen, setPrefixSeen] = useState(0);
  // The order nodes were first seen in, which colours the prefixes of a global service.
  const nodeOrder = useRef(new Map<string, number>());
  const theme = useTheme();
  const version = useSyncExternalStore(buffer.subscribe, buffer.getVersion);
  const scrollRef = useRef<HTMLDivElement>(null);
  // The log surface reaches the bottom of the window, measured, so the page does not
  // scroll as well as the log; measured again when a notice above or below it comes or goes.
  // biome-ignore lint/correctness/useExhaustiveDependencies: these change what sits around the surface
  useLayoutEffect(() => fitToViewport(scrollRef.current, 320), [state.status]);
  const following = useRef(true);
  const [paused, setPaused] = useState(false);
  const pausedAt = useRef(0);

  // ---- the choices: address first, then what this browser remembers ----
  const prefs = useMemo(loadLogPrefs, []);
  const [own, setOwn] = useState<LogViewParams>(params ?? {});
  const view = onParamsChange ? (params ?? {}) : own;
  const change = (patch: Partial<LogViewParams>) => {
    const next: LogViewParams = { ...view, ...patch };
    for (const k of Object.keys(next) as Array<keyof LogViewParams>) {
      if (next[k] === undefined) delete next[k];
    }
    if (onParamsChange) onParamsChange(next);
    else setOwn(next);
  };
  const savedTaskNames = useMemo(loadTaskNames, []);
  const taskNamesOn = isService && (view.prefix ?? savedTaskNames);
  const setTaskNames = (on: boolean) => {
    saveTaskNames(on);
    change({ prefix: on });
  };
  const wrap = view.wrap ?? prefs.wrap;
  const timestamps = view.ts ?? prefs.timestamps;
  const utc = (view.utc ?? prefs.utc) && timestamps;
  const setView = (patch: { wrap?: boolean; timestamps?: boolean; utc?: boolean }) => {
    const now = { wrap, timestamps, utc: view.utc ?? prefs.utc, ...patch };
    saveLogPrefs(now);
    change({ wrap: now.wrap, ts: now.timestamps, utc: now.utc });
  };
  const sinceKey = view.since ?? "";
  const taskFilter = view.task ?? "";
  const nodeFilter = view.node ?? "";
  const stoppedOn = view.stopped === true;
  const filtersKey = `${taskFilter}|${nodeFilter}|${stoppedOn}`;
  // The filters of the stream whose lines the buffer holds. When the address changes them, the
  // lines already loaded are narrowed at once while the stream for the new set opens, and the
  // buffer is replaced by that stream's last 500 lines when it answers (D-33).
  const [loadedFilters, setLoadedFilters] = useState(filtersKey);
  const reloading = isService && loadedFilters !== filtersKey;
  const taskIds = useMemo(() => listOf(taskFilter), [taskFilter]);
  const nodeNames = useMemo(() => listOf(nodeFilter), [nodeFilter]);
  // The effect below tells a change of filters from every other change by what else is the same.
  const streamBase = useRef("");
  const show = useMemo(() => parseShow(sinceKey || undefined), [sinceKey]);

  // ---- the stream ----
  const [startReached, setStartReached] = useState(false);
  const [earlier, setEarlier] = useState<"idle" | "loading" | "error">("idle");
  useEffect(() => {
    // Only the filters changed: keep the lines (narrowed) until the new stream answers.
    const base = `${envId}|${kind}|${sourceId}|${containerNode ?? ""}|${sinceKey}|${openCount}`;
    const reload = kind === "service" && streamBase.current === base && buffer.length > 0;
    streamBase.current = base;
    following.current = true;
    setPaused(false);
    if (!reload) {
      buffer.clear();
      setStartReached(false);
      setEarlier("idle");
      setUnreadable(null);
      setTasks(null);
      prefixPairs.current = new Map();
      setPrefixSeen(0);
      nodeOrder.current = new Map();
      setLoadedFilters(filtersKey);
    }
    everLive.current = false;
    let firstReady = true;
    const range = streamRange(parseShow(sinceKey || undefined), new Date());
    // Notes the slot and node pairs the lines name, and the order nodes first appear in.
    const noteTasks = (lines: readonly LogLineData[]) => {
      let added = false;
      for (const l of lines) {
        if (l.task === undefined) continue;
        const slot = l.slot ?? 0;
        const node = l.node ?? "";
        const key = `${slot}@${node}`;
        if (!prefixPairs.current.has(key)) {
          prefixPairs.current.set(key, { slot, node });
          added = true;
        }
        if (slot === 0 && !nodeOrder.current.has(node)) {
          nodeOrder.current.set(node, nodeOrder.current.size);
        }
      }
      if (added) setPrefixSeen(prefixPairs.current.size);
    };
    const target: StreamTarget =
      kind === "service"
        ? { source: { kind: "service", id: sourceId } }
        : { containerId: sourceId, nodeId: containerNode };
    const stream = createLogStream({
      envId,
      ...target,
      sid,
      ...range,
      service:
        kind === "service"
          ? { tasks: listOf(taskFilter), nodes: listOf(nodeFilter), stopped: stoppedOn }
          : undefined,
      onReady: (info) => {
        if (firstReady) {
          firstReady = false;
          if (reload) buffer.clear();
          setLoadedFilters(filtersKey);
        }
        setTasks(info.tasks ?? null);
      },
      onTasks: setTasks,
      onMarker: ({ at, ...mark }) => buffer.insertSwarmMarker(mark, at ?? new Date().toISOString()),
      onNode: ({ at, ...notice }) =>
        buffer.insertSwarmMarker(notice, at ?? new Date().toISOString()),
      onLines: (lines) => {
        if (kind === "service") noteTasks(lines);
        buffer.push(lines);
      },
      onState: (next) => {
        if (next.status === "live") everLive.current = true;
        setState(next);
      },
      onUnreadable: setUnreadable,
      onInterrupted: () => {
        // Told once: a stream that cannot be resumed closes.
        if (buffer.last?.marker !== "interrupted")
          buffer.pushMarker("interrupted", new Date().toISOString());
      },
      // The end of a service's stream is not a container that stopped; there is no row for it.
      onContainerState:
        kind === "service"
          ? undefined
          : ({ state: what, at }) => {
              // A reconnect to a stopped container is told again; the row is shown once.
              if (what === "stopped" && buffer.last?.marker !== "stopped")
                buffer.pushMarker("stopped", at);
              else if (what === "restarted") buffer.pushMarker("restarted", at);
            },
    });
    streamRef.current = stream;
    return () => {
      streamRef.current = null;
      stream.close();
    };
  }, [
    buffer,
    envId,
    kind,
    sourceId,
    containerNode,
    sid,
    sinceKey,
    taskFilter,
    nodeFilter,
    stoppedOn,
    filtersKey,
    openCount,
  ]);

  // A pop-out nobody can see lets its stream go after a minute and takes it up again
  // from the last line when it is seen (D-22). The Logs tab keeps its stream.
  useEffect(() => {
    if (!popout) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const onVisibility = () => {
      clearTimeout(timer);
      if (document.hidden) timer = setTimeout(() => streamRef.current?.suspend(), HIDDEN_POPOUT_MS);
      else streamRef.current?.resume();
    };
    document.addEventListener("visibilitychange", onVisibility);
    if (document.hidden) onVisibility();
    return () => {
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, [popout]);

  // ---- search ----
  const [queryInput, setQueryInput] = useState(view.q ?? "");
  const syncedQ = useRef(view.q ?? "");
  const [regex, setRegex] = useState(false);
  const [caseSensitive, setCaseSensitive] = useState(false);
  const [onlyMatching, setOnlyMatching] = useState(false);
  const [current, setCurrent] = useState(0);
  const query = useDebounced(queryInput, SEARCH_DEBOUNCE_MS);
  // The address changed under the box (back, forward, a pasted link).
  useEffect(() => {
    if ((view.q ?? "") !== syncedQ.current) {
      syncedQ.current = view.q ?? "";
      setQueryInput(view.q ?? "");
    }
  }, [view.q]);
  // The box changed: after a pause in typing the address follows.
  const settledQuery = useDebounced(queryInput, ANNOUNCE_DEBOUNCE_MS);
  // biome-ignore lint/correctness/useExhaustiveDependencies: only a settled query is written
  useEffect(() => {
    if (settledQuery !== syncedQ.current) {
      syncedQ.current = settledQuery;
      change({ q: settledQuery || undefined });
    }
  }, [settledQuery]);

  // biome-ignore lint/correctness/useExhaustiveDependencies: version is the trigger, not an input
  const result = useMemo(
    () => searchLines(buffer, query, { regex, caseSensitive }),
    [buffer, version, query, regex, caseSensitive],
  );
  const matches = result.matches;
  const invalidPattern = result.error === "invalid_pattern";
  const cur = matches.length === 0 ? 0 : Math.min(current, matches.length - 1);
  const matchByIndex = useMemo(() => new Map(matches.map((m) => [m.index, m])), [matches]);
  const currentLine = matches[cur]?.index;

  // ---- the rows: what is drawn, in order ----
  // A code is 2 x the index of a line in the buffer for the day divider before it and
  // 2 x index + 1 for the line itself, so the codes of a log are sorted.
  const filtering = onlyMatching && query !== "" && !invalidPattern;
  // biome-ignore lint/correctness/useExhaustiveDependencies: version is the trigger, not an input
  const codes = useMemo(() => {
    const out: number[] = [];
    let day = "";
    const add = (i: number) => {
      const line = buffer.at(i);
      if (reloading && !shownUnderFilters(line, taskIds, nodeNames)) return;
      if (timestamps && !line.marker) {
        const d = stampDay(line.t, utc);
        if (d !== day) {
          day = d;
          out.push(i * 2);
        }
      }
      out.push(i * 2 + 1);
    };
    if (filtering) for (const m of matches) add(m.index);
    else for (let i = 0; i < buffer.length; i++) add(i);
    return out;
  }, [buffer, version, filtering, matches, timestamps, utc, reloading, taskIds, nodeNames]);

  const capped = buffer.droppedCount > 0;
  const allLoaded = show.kind === "all" && buffer.length < 50_000;
  const hasOldest = !isService && buffer.length > 0;
  const topRow = hasOldest
    ? capped
      ? "cap"
      : startReached || allLoaded
        ? "start"
        : earlier === "error"
          ? "error"
          : "load"
    : null;
  const topCount = topRow ? 1 : 0;
  const count = topCount + codes.length;

  const virtualizer = useVirtualizer({
    count,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => LINE_HEIGHT_PX,
    overscan: OVERSCAN,
    getItemKey: (index) => {
      if (index < topCount) return "top";
      const code = codes[index - topCount];
      if (code === undefined || code >> 1 >= buffer.length) return `gone-${index}`;
      const seq = buffer.at(code >> 1).seq;
      return (code & 1) === 0 ? `day-${seq}` : seq;
    },
  });
  // Wrapped lines have their own heights; unwrapping makes them all one again.
  // biome-ignore lint/correctness/useExhaustiveDependencies: wrap is the trigger
  useEffect(() => {
    virtualizer.measure();
  }, [wrap, virtualizer]);

  // Where a line sits in the rows, to scroll a match into view.
  const rowOf = useCallback(
    (bufferIndex: number): number => {
      const want = bufferIndex * 2 + 1;
      let lo = 0;
      let hi = codes.length - 1;
      while (lo <= hi) {
        const mid = (lo + hi) >> 1;
        const c = codes[mid] as number;
        if (c === want) return mid + topCount;
        if (c < want) lo = mid + 1;
        else hi = mid - 1;
      }
      return -1;
    },
    [codes, topCount],
  );
  const scrollToMatch = (bufferIndex: number | undefined) => {
    if (bufferIndex === undefined) return;
    const row = rowOf(bufferIndex);
    if (row >= 0) virtualizer.scrollToIndex(row, { align: "center" });
  };
  // A new search, or a new way of searching, goes to its first match.
  // biome-ignore lint/correctness/useExhaustiveDependencies: only the search itself is the trigger
  useEffect(() => {
    setCurrent(0);
    scrollToMatch(matches[0]?.index);
  }, [query, regex, caseSensitive, onlyMatching]);
  const step = (delta: number) => {
    if (matches.length < 2) return;
    const next = (cur + delta + matches.length) % matches.length;
    setCurrent(next);
    scrollToMatch(matches[next]?.index);
  };

  // ---- following, pausing and the pill ----
  const anchor = useRef<{ total: number; top: number } | null>(null);
  const jumpToLatest = () => {
    const el = scrollRef.current;
    following.current = true;
    pausedAt.current = buffer.pushed;
    setPaused(false);
    if (el) el.scrollTop = virtualizer.getTotalSize();
  };
  const total = virtualizer.getTotalSize();
  // Follow: while the viewer is at the end, move to the end after every change; the
  // browser clamps the position to the real end. A drop from the top changes the
  // buffer without changing its length, so the buffer's version, not its length, is
  // what triggers it. Lines added at the top (Load earlier) keep what the viewer was
  // looking at where it was.
  // biome-ignore lint/correctness/useExhaustiveDependencies: version is the trigger, not an input
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    if (anchor.current) {
      el.scrollTop = anchor.current.top + (total - anchor.current.total);
      anchor.current = null;
      return;
    }
    if (following.current && count > 0) el.scrollTop = total;
  }, [version, count, total]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    const atEnd = el.scrollHeight - el.scrollTop - el.clientHeight <= FOLLOW_SLACK_PX;
    if (atEnd && !following.current) {
      following.current = true;
      setPaused(false);
    } else if (!atEnd && following.current) {
      following.current = false;
      pausedAt.current = buffer.pushed;
      setPaused(true);
    }
  };
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "End") jumpToLatest();
  };
  const newLines = paused ? Math.max(0, buffer.pushed - pausedAt.current) : 0;

  // ---- earlier lines ----
  const loadEarlier = async () => {
    const oldest = (() => {
      for (let i = 0; i < buffer.length; i++) if (!buffer.at(i).marker) return buffer.at(i);
      return undefined;
    })();
    if (!oldest || earlier === "loading") return;
    setEarlier("loading");
    try {
      const page = await fetchLogPage({
        envId,
        containerId: sourceId,
        nodeId: containerNode,
        sid,
        until: plusNanosecond(oldest.t),
        tail: EARLIER_PAGE,
      });
      anchor.current = {
        total: virtualizer.getTotalSize(),
        top: scrollRef.current?.scrollTop ?? 0,
      };
      const added = buffer.prepend(page.lines);
      if (added === 0) anchor.current = null;
      if (page.startOfLogs || added === 0) setStartReached(true);
      setEarlier("idle");
    } catch {
      setEarlier("error");
    }
  };

  // ---- what is said ----
  const counter = counterText(
    { query, matchCount: matches.length, currentMatch: cur, invalidPattern },
    t,
  );
  const announcement = useDebounced(counter, ANNOUNCE_DEBOUNCE_MS);
  const markerLabel = (line: BufferedLine) =>
    line.marker === "swarm" && line.swarm
      ? swarmMarkerText(t, line.swarm, name)
      : line.marker === "interrupted"
        ? t("logs.interrupted")
        : t(line.marker === "stopped" ? "logs.stopped" : "logs.restarted", {
            time: clockTime(line.t),
          });
  const timeLabel = utc ? t("logs.timeUtc") : t("logs.time");
  const dayLabel = (iso: string) =>
    new Date(iso).toLocaleDateString(undefined, {
      dateStyle: "medium",
      timeZone: utc ? "UTC" : undefined,
    });

  const connected = state.status === "live" || state.status === "reconnecting";
  // Filters that leave no task to follow: said in place of "No log output yet".
  const noTaskMatches =
    isService && (taskFilter !== "" || nodeFilter !== "") && tasks !== null && tasks.length === 0;
  const loadFailed = state.status === "closed" && state.reason === "failed" && !everLive.current;
  const [downloadOpen, setDownloadOpen] = useState(false);
  const downloadTarget = { envId, containerId: sourceId, nodeId: containerNode };
  const onShowChange = (next: ShowRange) => change({ since: formatShow(next) });
  // The window opens with what is on screen, so it reads the same: the range, the search
  // and the toggles, as they are now (not only the ones the address holds).
  const openPopout = () => {
    const url = popoutLogsUrl(envId, sourceId, containerNode, {
      since: sinceKey || undefined,
      q: queryInput || undefined,
      ts: timestamps,
      utc: view.utc ?? prefs.utc,
      wrap,
    });
    window.open(url, "_blank", "popup,noopener,width=1100,height=760");
  };

  // ---- the task prefix of a service's lines ----
  const taskStates = useMemo(() => new Map((tasks ?? []).map((x) => [x.id, x.state])), [tasks]);
  // biome-ignore lint/correctness/useExhaustiveDependencies: prefixSeen is the trigger, not an input
  const prefixCh = useMemo(() => {
    let longest = 0;
    for (const { slot, node } of prefixPairs.current.values()) {
      longest = Math.max(
        longest,
        formatTaskPrefix({ service: name, slot, node, showService: showServiceName }).length,
      );
    }
    return prefixWidthCh(longest);
  }, [prefixSeen, name, showServiceName]);
  const prefixOf = (line: BufferedLine) => {
    if (!taskNamesOn || line.task === undefined) return undefined;
    const slot = line.slot ?? 0;
    const node = line.node ?? "";
    // A replicated service's tasks are coloured by slot, a global service's by node, so a
    // replacement task keeps its slot's colour.
    const colour = prefixColour(slot === 0 ? (nodeOrder.current.get(node) ?? 0) : slot - 1);
    return (
      <TaskPrefix
        text={formatTaskPrefix({ service: name, slot, node, showService: showServiceName })}
        colour={colour}
        widthCh={prefixCh}
        taskId={line.task}
        state={taskStates.get(line.task)}
      />
    );
  };

  const renderRow = (index: number) => {
    if (index < topCount) {
      return (
        <div className="flex h-5 w-full min-w-full items-center justify-center px-3 text-xs">
          {topRow === "cap" ? (
            <span className="text-muted-foreground">{t("logs.bufferCap")}</span>
          ) : topRow === "start" ? (
            <span className="text-muted-foreground">{t("logs.startOfLogs")}</span>
          ) : topRow === "error" ? (
            <span className="text-muted-foreground">
              {t("logs.loadError.title")} {t("logs.loadError.body")}{" "}
              <Button
                type="button"
                variant="link"
                size="xs"
                className="h-5 p-0 text-xs"
                onClick={loadEarlier}
              >
                {t("logs.loadError.retry")}
              </Button>
            </span>
          ) : (
            <Button
              type="button"
              variant="link"
              size="xs"
              className="h-5 p-0 text-xs"
              disabled={earlier === "loading"}
              onClick={loadEarlier}
            >
              {earlier === "loading" ? t("logs.loadingEarlier") : t("logs.loadEarlier")}
            </Button>
          )}
        </div>
      );
    }
    const code = codes[index - topCount];
    if (code === undefined || code >> 1 >= buffer.length) return null;
    const line = buffer.at(code >> 1);
    if ((code & 1) === 0) return <DayDividerRow label={dayLabel(line.t)} />;
    const hit = matchByIndex.get(code >> 1);
    return (
      <LogLineRow
        line={line}
        stderrLabel={t("logs.stderr")}
        markerLabel={markerLabel}
        time={timestamps ? { utc, label: timeLabel } : undefined}
        wrap={wrap}
        theme={theme}
        prefix={prefixOf(line)}
        highlight={hit ? { ranges: hit.ranges, current: code >> 1 === currentLine } : undefined}
      />
    );
  };

  // The notice takes the place of the viewer and its toolbar: the tab keeps its place.
  if (unreadable) return <UnreadableNotice info={unreadable} />;

  return (
    <div className="flex flex-col gap-2">
      <LogToolbar
        show={show}
        onShowChange={onShowChange}
        query={queryInput}
        onQueryChange={setQueryInput}
        regex={regex}
        onRegexChange={setRegex}
        caseSensitive={caseSensitive}
        onCaseSensitiveChange={setCaseSensitive}
        onlyMatching={onlyMatching}
        onOnlyMatchingChange={setOnlyMatching}
        invalidPattern={invalidPattern}
        matchCount={matches.length}
        currentMatch={cur}
        onNextMatch={() => step(1)}
        onPreviousMatch={() => step(-1)}
        announcement={announcement}
        wrap={wrap}
        onWrapChange={(on) => setView({ wrap: on })}
        timestamps={timestamps}
        onTimestampsChange={(on) => setView({ timestamps: on })}
        utc={utc}
        onUtcChange={(on) => setView({ utc: on })}
        taskNames={isService ? { on: taskNamesOn, onChange: setTaskNames } : undefined}
        following={
          isService && tasks ? tasks.filter((x) => !isEndedTaskState(x.state)).length : undefined
        }
        filterBar={
          isService ? (
            <TaskFilterBar
              service={name}
              options={filterOptions}
              tasks={taskIds}
              nodes={nodeNames}
              stopped={stoppedOn}
              onTasksChange={(ids) => change({ task: ids.join(",") || undefined })}
              onNodesChange={(names) => change({ node: names.join(",") || undefined })}
              onStoppedChange={(on) => change({ stopped: on || undefined })}
            />
          ) : undefined
        }
        live={connected ? (state.status === "live" ? "live" : "reconnecting") : undefined}
        actions={
          isService ? (
            actions
          ) : (
            <>
              <DownloadPopover target={downloadTarget} show={show} timestamps={timestamps} />
              {actions}
            </>
          )
        }
        onOpenPopout={popout || isService ? undefined : openPopout}
        moreItems={
          isService ? (
            moreItems
          ) : (
            <>
              <DropdownMenuItem onSelect={() => setDownloadOpen(true)}>
                {t("logs.download.trigger")}
              </DropdownMenuItem>
              {moreItems}
            </>
          )
        }
      />
      {isService ? null : (
        <DownloadDialog
          open={downloadOpen}
          onOpenChange={setDownloadOpen}
          target={downloadTarget}
          show={show}
          timestamps={timestamps}
        />
      )}
      <div className="relative">
        <div
          ref={scrollRef}
          role="log"
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrollable region must take focus so the keys scroll it (UI-SPEC "Surface")
          tabIndex={0}
          aria-live="off"
          aria-label={t("logs.surface", { name })}
          aria-busy={state.status === "connecting"}
          onScroll={onScroll}
          onKeyDown={onKeyDown}
          className="min-h-80 overflow-auto rounded-lg border bg-card focus-visible:ring-2 focus-visible:ring-accent-ink focus-visible:outline-none"
        >
          {loadFailed ? (
            <div
              role="alert"
              className="flex h-full flex-col items-center justify-center gap-2 p-6 text-center"
            >
              <p className="font-medium">{t("logs.loadError.title")}</p>
              <p className="text-sm text-muted-foreground">{t("logs.loadError.body")}</p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setOpenCount((n) => n + 1)}
              >
                {t("logs.loadError.retry")}
              </Button>
            </div>
          ) : state.status === "connecting" && !(reloading && buffer.length > 0) ? (
            <div className="flex flex-col gap-1.5 p-3">
              <span role="status" className="sr-only">
                {t("logs.loading")}
              </span>
              {SKELETON_WIDTHS.map((w, i) => (
                // The widths repeat, so the position is the only identity a skeleton has.
                // biome-ignore lint/suspicious/noArrayIndexKey: static decoration
                <Skeleton key={i} className={`h-3 ${w}`} />
              ))}
            </div>
          ) : (
            <div className="relative min-w-full" style={{ height: total }}>
              {virtualizer.getVirtualItems().map((row) => (
                <div
                  key={row.key}
                  ref={wrap ? virtualizer.measureElement : undefined}
                  data-index={row.index}
                  className="absolute top-0 left-0 min-w-full"
                  style={{
                    transform: `translateY(${row.start}px)`,
                    ...(wrap ? { width: "100%" } : {}),
                  }}
                >
                  {renderRow(row.index)}
                </div>
              ))}
            </div>
          )}
        </div>
        {connected && buffer.length === 0 ? (
          isService ? (
            <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-1 p-6 text-center text-sm text-muted-foreground">
              {noTaskMatches ? (
                <p>{t("logs.service.noTasksMatch")}</p>
              ) : (
                <>
                  <p>{t("logs.empty")}</p>
                  <p className="text-xs">{t("logs.service.emptyBody")}</p>
                </>
              )}
            </div>
          ) : (
            <p className="pointer-events-none absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">
              {t("logs.empty")}
            </p>
          )
        ) : null}
        {paused ? (
          <div className="pointer-events-none absolute inset-x-0 bottom-4 flex justify-center">
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="pointer-events-auto rounded-full bg-background shadow-xs max-md:h-11"
              onClick={jumpToLatest}
            >
              {newLines > 0 ? t("logs.jumpNew", { count: newLines }) : t("logs.jump")}
            </Button>
          </div>
        ) : null}
        <span role="status" aria-live="polite" className="sr-only">
          {paused ? t("logs.paused") : ""}
        </span>
      </div>
      {state.status === "closed" && state.reason === "access_lost" ? (
        <p role="status" className="rounded-lg border bg-muted px-3 py-2 text-sm">
          {t("logs.accessLost")}
        </p>
      ) : null}
    </div>
  );
}
