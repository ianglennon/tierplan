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
import { type BufferedLine, clockTime, LogBuffer, stampDay } from "@/features/logs/log-buffer";
import { DayDividerRow, LINE_HEIGHT_PX, LogLineRow } from "@/features/logs/log-line";
import {
  formatShow,
  type LogViewParams,
  loadLogPrefs,
  parseShow,
  type ShowRange,
  saveLogPrefs,
  streamRange,
} from "@/features/logs/log-prefs";
import {
  createLogStream,
  fetchLogPage,
  type LogStreamState,
  makeViewerId,
  plusNanosecond,
  type UnreadableInfo,
} from "@/features/logs/log-stream";
import { counterText, LogToolbar } from "@/features/logs/log-toolbar";
import { searchLines } from "@/features/logs/search";
import { UnreadableNotice } from "@/features/logs/unreadable-notice";
import { useT } from "@/i18n/use-t";

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

export interface LogViewerProps {
  envId: string;
  containerId: string;
  nodeId?: string;
  // The container's name, for the accessible name of the surface.
  name: string;
  // The range, search and toggles from the address (D-22). With onParamsChange the
  // address owns them; without it the viewer keeps them itself.
  params?: LogViewParams;
  onParamsChange?: (params: LogViewParams) => void;
  // Controls after Download… at the right of the toolbar's second row, and the items of
  // the More menu that stand in for them on a narrow screen.
  actions?: ReactNode;
  moreItems?: ReactNode;
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
export function LogViewer({
  envId,
  containerId,
  nodeId,
  name,
  params,
  onParamsChange,
  actions,
  moreItems,
}: LogViewerProps) {
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
  // Set when the engine cannot read this container's log back (D-25).
  const [unreadable, setUnreadable] = useState<UnreadableInfo | null>(null);
  const theme = useTheme();
  const version = useSyncExternalStore(buffer.subscribe, buffer.getVersion);
  const scrollRef = useRef<HTMLDivElement>(null);
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
  const wrap = view.wrap ?? prefs.wrap;
  const timestamps = view.ts ?? prefs.timestamps;
  const utc = (view.utc ?? prefs.utc) && timestamps;
  const setView = (patch: { wrap?: boolean; timestamps?: boolean; utc?: boolean }) => {
    const now = { wrap, timestamps, utc: view.utc ?? prefs.utc, ...patch };
    saveLogPrefs(now);
    change({ wrap: now.wrap, ts: now.timestamps, utc: now.utc });
  };
  const sinceKey = view.since ?? "";
  const show = useMemo(() => parseShow(sinceKey || undefined), [sinceKey]);

  // ---- the stream ----
  const [startReached, setStartReached] = useState(false);
  const [earlier, setEarlier] = useState<"idle" | "loading" | "error">("idle");
  // biome-ignore lint/correctness/useExhaustiveDependencies: openCount is the Retry trigger
  useEffect(() => {
    buffer.clear();
    following.current = true;
    setPaused(false);
    setStartReached(false);
    setEarlier("idle");
    setUnreadable(null);
    everLive.current = false;
    const range = streamRange(parseShow(sinceKey || undefined), new Date());
    const stream = createLogStream({
      envId,
      containerId,
      nodeId,
      sid,
      ...range,
      onLines: (lines) => buffer.push(lines),
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
      onContainerState: ({ state: what, at }) => {
        // A reconnect to a stopped container is told again; the row is shown once.
        if (what === "stopped" && buffer.last?.marker !== "stopped")
          buffer.pushMarker("stopped", at);
        else if (what === "restarted") buffer.pushMarker("restarted", at);
      },
    });
    return () => stream.close();
  }, [buffer, envId, containerId, nodeId, sid, sinceKey, openCount]);

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
  }, [buffer, version, filtering, matches, timestamps, utc]);

  const capped = buffer.droppedCount > 0;
  const allLoaded = show.kind === "all" && buffer.length < 50_000;
  const hasOldest = buffer.length > 0;
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
        containerId,
        nodeId,
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
    line.marker === "interrupted"
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
  const loadFailed = state.status === "closed" && state.reason === "failed" && !everLive.current;
  const [downloadOpen, setDownloadOpen] = useState(false);
  const downloadTarget = { envId, containerId, nodeId };
  const onShowChange = (next: ShowRange) => change({ since: formatShow(next) });

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
        live={connected ? (state.status === "live" ? "live" : "reconnecting") : undefined}
        actions={
          <>
            <DownloadPopover target={downloadTarget} show={show} timestamps={timestamps} />
            {actions}
          </>
        }
        moreItems={
          <>
            <DropdownMenuItem onSelect={() => setDownloadOpen(true)}>
              {t("logs.download.trigger")}
            </DropdownMenuItem>
            {moreItems}
          </>
        }
      />
      <DownloadDialog
        open={downloadOpen}
        onOpenChange={setDownloadOpen}
        target={downloadTarget}
        show={show}
        timestamps={timestamps}
      />
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
          className="h-[calc(100svh-20rem)] min-h-80 overflow-auto rounded-lg border bg-card focus-visible:ring-2 focus-visible:ring-accent-ink focus-visible:outline-none"
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
          ) : state.status === "connecting" ? (
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
          <p className="pointer-events-none absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">
            {t("logs.empty")}
          </p>
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
