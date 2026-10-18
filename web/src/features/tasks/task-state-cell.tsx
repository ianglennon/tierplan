// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type TaskItem, taskState } from "@/features/tasks/task-state";
import { useT } from "@/i18n/use-t";

interface TaskStateCellProps {
  task: Pick<TaskItem, "state" | "dockerState" | "stateSince">;
}

// TaskStateCell is a task's State column: the state's dot and word, and how long ago it began.
// A state the server changed to unknown (the task's node cannot vouch for it) says in a tooltip
// what Docker last recorded. The words are text nodes.
export function TaskStateCell({ task }: TaskStateCellProps) {
  const t = useT();
  const { tone, labelKey } = taskState(task.state);
  const word = (
    <span data-slot="task-state-word">
      <StatusDot state={tone} label={t(labelKey)} />
    </span>
  );
  const unconfirmed = task.state !== task.dockerState;
  return (
    <span
      data-slot="task-state"
      className="inline-flex flex-wrap items-center gap-x-2 whitespace-nowrap"
    >
      {unconfirmed ? (
        <Tooltip>
          <TooltipTrigger asChild>{word}</TooltipTrigger>
          <TooltipContent>
            {t("tasks.state.unknownTooltip", { state: t(taskState(task.dockerState).labelKey) })}
          </TooltipContent>
        </Tooltip>
      ) : (
        word
      )}
      <span className="text-xs text-muted-foreground">
        <RelativeTime value={task.stateSince} />
      </span>
    </span>
  );
}
