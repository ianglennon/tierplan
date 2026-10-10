// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useVirtualizer } from "@tanstack/react-virtual";
import { useEffect, useLayoutEffect, useRef, useState, useSyncExternalStore } from "react";

import { LiveChip } from "@/components/live-chip";
import { Skeleton } from "@/components/ui/skeleton";
import { type BufferedLine, clockTime, LogBuffer } from "@/features/logs/log-buffer";
import { LINE_HEIGHT_PX, LogLineRow } from "@/features/logs/log-line";
import { createLogStream, type LogStreamState, makeViewerId } from "@/features/logs/log-stream";
import { useT } from "@/i18n/use-t";

// How many lines a viewer opens on (D-15).
const OPEN_TAIL = 500;
// Rows drawn beyond the visible ones, so a fast scroll does not show a blank.
const OVERSCAN = 20;
// How far from the end still counts as being at the end, in pixels.
const FOLLOW_SLACK_PX = 40;
// The widths of the skeleton lines shown until the stream connects.
const SKELETON_WIDTHS = ["w-3/5", "w-4/5", "w-2/5", "w-3/4", "w-1/2", "w-2/3", "w-4/5", "w-1/3"];

export interface LogViewerProps {
  envId: string;
  containerId: string;
  nodeId?: string;
  // The container's name, for the accessible name of the surface.
  name: string;
}

// LogViewer shows a container's log live: it opens on the last 500 lines and follows,
// drawing only the rows in view (a virtualised list of 20px rows). Scrolling away from
// the end stops the following until the end is reached again. The lines live in a
// buffer of at most 50 000.
export function LogViewer({ envId, containerId, nodeId, name }: LogViewerProps) {
  const t = useT();
  // The viewer's own ID: made once, sent on every stream of this viewer, so the
  // server audits it once however often the stream reconnects (D-24).
  const [sid] = useState(makeViewerId);
  const [buffer] = useState(() => new LogBuffer());
  const [state, setState] = useState<LogStreamState>({ status: "connecting" });
  const version = useSyncExternalStore(buffer.subscribe, buffer.getVersion);
  const scrollRef = useRef<HTMLDivElement>(null);
  const following = useRef(true);

  useEffect(() => {
    buffer.clear();
    following.current = true;
    const stream = createLogStream({
      envId,
      containerId,
      nodeId,
      sid,
      tail: OPEN_TAIL,
      onLines: (lines) => buffer.push(lines),
      onState: setState,
      onContainerState: ({ state: what, at }) => {
        // A reconnect to a stopped container is told again; the row is shown once.
        if (what === "stopped" && buffer.last?.marker !== "stopped")
          buffer.pushMarker("stopped", at);
        else if (what === "restarted") buffer.pushMarker("restarted", at);
      },
    });
    return () => stream.close();
  }, [buffer, envId, containerId, nodeId, sid]);

  const count = buffer.length;
  const virtualizer = useVirtualizer({
    count,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => LINE_HEIGHT_PX,
    overscan: OVERSCAN,
    getItemKey: (index) => buffer.at(index).seq,
  });

  // Follow: while the viewer is at the end, move to the end after every change of the
  // buffer; the browser clamps the position to the real end. A drop from the top
  // changes the buffer without changing its length, so the buffer's version, not its
  // length, is what triggers it.
  // biome-ignore lint/correctness/useExhaustiveDependencies: version is the trigger, not an input
  useLayoutEffect(() => {
    const el = scrollRef.current;
    if (el && following.current && count > 0) el.scrollTop = virtualizer.getTotalSize();
  }, [version, count, virtualizer]);

  const onScroll = () => {
    const el = scrollRef.current;
    if (!el) return;
    following.current = el.scrollHeight - el.scrollTop - el.clientHeight <= FOLLOW_SLACK_PX;
  };

  const markerLabel = (line: BufferedLine) =>
    t(line.marker === "stopped" ? "logs.stopped" : "logs.restarted", { time: clockTime(line.t) });
  const connected = state.status === "live" || state.status === "reconnecting";
  return (
    <div className="flex flex-col gap-2">
      <div className="flex min-h-6 items-center justify-end">
        {connected ? <LiveChip state={state.status === "live" ? "live" : "reconnecting"} /> : null}
      </div>
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
          className="h-[calc(100svh-16rem)] min-h-80 overflow-auto rounded-lg border bg-card focus-visible:ring-2 focus-visible:ring-accent-ink focus-visible:outline-none"
        >
          {state.status === "connecting" ? (
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
            <div className="relative min-w-full" style={{ height: virtualizer.getTotalSize() }}>
              {virtualizer.getVirtualItems().map((row) =>
                row.index < buffer.length ? (
                  <div
                    key={row.key}
                    className="absolute top-0 left-0 min-w-full"
                    style={{ transform: `translateY(${row.start}px)` }}
                  >
                    <LogLineRow
                      line={buffer.at(row.index)}
                      stderrLabel={t("logs.stderr")}
                      markerLabel={markerLabel}
                    />
                  </div>
                ) : null,
              )}
            </div>
          )}
        </div>
        {connected && count === 0 ? (
          <p className="pointer-events-none absolute inset-0 flex items-center justify-center text-sm text-muted-foreground">
            {t("logs.empty")}
          </p>
        ) : null}
      </div>
      {state.status === "closed" && state.reason === "access_lost" ? (
        <p role="status" className="rounded-lg border bg-muted px-3 py-2 text-sm">
          {t("logs.accessLost")}
        </p>
      ) : null}
    </div>
  );
}
