// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { isSlotMark, type SwarmMark } from "@/features/logs/log-source";

export type Translate = (key: string, options?: Record<string, unknown>) => string;

// A task ID is long and says little of itself; its first eight characters name it, as they
// do in the Tasks tab and in a filter.
const shortId = (id: string): string => id.slice(0, 8);

// What happened to the old task, in words: a failed task with its exit code when it has one,
// a task that shut down, a task that was rejected. A state it does not know is shown as sent.
function whatHappened(t: Translate, state: string, exitCode: number | undefined): string {
  switch (state) {
    case "failed":
      return exitCode === undefined
        ? t("logs.marker.what.failed")
        : t("logs.marker.what.failedExit", { code: exitCode });
    case "shutdown":
      return t("logs.marker.what.shutdown");
    case "rejected":
      return t("logs.marker.what.rejected");
    default:
      return state;
  }
}

// swarmMarkerText is what a marker row says (plan 06-30, D-34, D-37; UI-SPEC "Replacement
// marker", "Node gap"): a task was replaced, a slot is waiting for a task, or a node cannot be
// reached or is back. A slot is named {service}.{slot}; the task of a global service, which has
// no slot, by the service and its node.
export function swarmMarkerText(t: Translate, mark: SwarmMark, service: string): string {
  if (!isSlotMark(mark)) return t(`logs.nodeNotice.${mark.kind}`, { node: mark.node });
  const label = mark.slot > 0 ? `${service}.${mark.slot}` : `${service}@${mark.oldNode}`;
  const what = whatHappened(t, mark.oldState, mark.exitCode);
  if (mark.kind === "stopped") {
    return t("logs.marker.stopped", { label, old: shortId(mark.old), what });
  }
  return t("logs.marker.replaced", {
    label,
    old: shortId(mark.old),
    what,
    new: shortId(mark.new ?? ""),
    node: mark.node ?? "",
  });
}

export interface ReplacementMarkerProps {
  text: string;
  kind?: string;
}

// ReplacementMarker is the centred, muted row between lines where a task was replaced, a slot
// is waiting for a task or a node cannot be reached: a separator whose text is its accessible
// name. The log surface is aria-live off, so it is not announced as it arrives. The text is
// built from IDs, names and numbers and drawn as text, never as markup (T-06-89).
export function ReplacementMarker({ text, kind }: ReplacementMarkerProps) {
  return (
    // biome-ignore lint/a11y/useSemanticElements: an <hr> cannot hold the text between its two rules
    <div
      role="separator"
      aria-label={text}
      data-swarm-marker={kind}
      className="flex h-5 w-full min-w-full items-center gap-3 px-3 text-xs text-muted-foreground"
    >
      <span aria-hidden="true" className="h-px flex-1 bg-border" />
      <span aria-hidden="true" title={text} className="min-w-0 shrink truncate">
        {text}
      </span>
      <span aria-hidden="true" className="h-px flex-1 bg-border" />
    </div>
  );
}
