// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import type { StackTarget } from "@/features/logs/log-source";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The task prefix of a line of a service's log (plan 06-29, OBS-02, D-32; UI-SPEC "Task
// prefix palette"). It names the task that wrote the line, in a colour that only helps
// scanning: the text is always there and unique per task.

// The eight colours, from the page's ANSI palette, that keep 4.5:1 on the card in both
// themes (measured in Phase 4). Red and green, and their bright forms, are left out on
// purpose so a prefix never looks like a status.
export const PREFIX_COLOURS = [4, 5, 6, 3, 12, 13, 14, 11].map((n) => `var(--ansi-${n})`);

// prefixColour is the colour of the n-th task: a slot's number less one for a replicated
// service, the order a node was first seen in for a global one. It wraps after eight.
export function prefixColour(index: number): string {
  const n = PREFIX_COLOURS.length;
  return PREFIX_COLOURS[((Math.trunc(index) % n) + n) % n] as string;
}

// The gutter is as wide as the longest prefix loaded, from 12 to 28 characters; a longer
// prefix is cut and shows in full in a tooltip.
export const MIN_PREFIX_CH = 12;
export const MAX_PREFIX_CH = 28;

export function prefixWidthCh(longest: number): number {
  return Math.min(MAX_PREFIX_CH, Math.max(MIN_PREFIX_CH, longest));
}

export interface PrefixParts {
  service: string;
  // The task's slot; 0 for a task of a global service.
  slot: number;
  // The host name of the task's node; empty when it is not known.
  node: string;
  // True where the service's name is not already on screen (the pop-out).
  showService: boolean;
}

// formatTaskPrefix is 2@node3 beside the service's header, web.2@node3 where the name is
// needed, and web@node3 for a task of a global service, whose slot says nothing.
export function formatTaskPrefix({ service, slot, node, showService }: PrefixParts): string {
  const name = slot === 0 ? service : showService ? `${service}.${slot}` : String(slot);
  return node ? `${name}@${node}` : name;
}

export interface StackPrefixParts {
  target: StackTarget;
  // The service's name in the compose file, without the stack's prefix.
  service: string;
  // A swarm task's slot (0 for a task of a global service), or a container's number.
  slot: number;
  node: string;
}

// formatStackPrefix is the prefix of a line of a stack's log (plan 07-29, D-22): web.2@node3 for a
// task of a swarm, web@node3 for a task of a global service, and web-2 for the second container
// of a service on a standalone host, which has one node and needs no name for it.
export function formatStackPrefix({ target, service, slot, node }: StackPrefixParts): string {
  if (target === "standalone") return `${service}-${slot}`;
  return formatTaskPrefix({ service, slot, node, showService: true });
}

export interface TaskPrefixProps {
  text: string;
  colour: string;
  widthCh: number;
  taskId: string;
  state?: string;
}

// TaskPrefix draws the prefix as text in its colour: it cannot be selected, so a copy of
// the lines is clean, and it is real text for a screen reader. Over 28 characters it is cut,
// and a tooltip, which a keyboard reaches too, shows the whole prefix, the task's ID and
// its state.
export function TaskPrefix({ text, colour, widthCh, taskId, state }: TaskPrefixProps) {
  const t = useT();
  const cut = text.length > MAX_PREFIX_CH;
  const prefix = (
    <span
      data-task-prefix=""
      // A cut prefix is a control a keyboard can reach, so the tooltip is not for a mouse alone.
      tabIndex={cut ? 0 : undefined}
      className={cn(
        "shrink-0 select-none overflow-hidden font-mono text-xs font-semibold whitespace-pre",
        cut && "truncate",
      )}
      style={{ color: colour, width: `${widthCh}ch` }}
    >
      {text}
    </span>
  );
  if (!cut) return prefix;
  return (
    <Tooltip>
      <TooltipTrigger asChild>{prefix}</TooltipTrigger>
      <TooltipContent className="flex max-w-sm flex-col gap-0.5 break-all font-mono">
        <span>{text}</span>
        <span>{t("logs.prefix.task", { id: taskId })}</span>
        {state ? <span>{t("logs.prefix.state", { state })}</span> : null}
      </TooltipContent>
    </Tooltip>
  );
}
