// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// One line of the list: a container (or volume) a dialog is about to act on.
export interface TargetRow {
  name: string;
  node?: string;
  // "Task of service web", "Part of project shop", "Not running, skipped".
  note?: string;
  // Not acted on: shown muted.
  skipped?: boolean;
}

interface TargetListProps {
  rows: readonly TargetRow[];
  // On a swarm the list says which node each one is on.
  swarm: boolean;
  // The column header of the first column; "Container" unless told.
  label?: string;
  // The headers of the second column (shown on a swarm) and the third: "Node" and "Note"
  // unless told, so a list of tasks can say Service and What happens.
  nodeLabel?: string;
  noteLabel?: string;
}

// TargetList is the table inside a confirmation dialog that names everything the
// action will touch: Container, Node (swarm only) and Note, or the headers it is told. It scrolls inside a
// 240px maximum; a long name is cut with an ellipsis and whole in a tooltip.
// Names are Docker's, so they are text only.
export function TargetList({ rows, swarm, label, nodeLabel, noteLabel }: TargetListProps) {
  const t = useT();
  return (
    <div
      data-slot="target-list"
      className="max-h-60 overflow-y-auto rounded-lg ring-1 ring-foreground/10"
    >
      <table className="w-full text-sm">
        <thead className="sticky top-0 bg-muted text-xs text-muted-foreground">
          <tr>
            <th scope="col" className="px-2 py-1 text-start font-semibold">
              {label ?? t("actions.targets.container")}
            </th>
            {swarm ? (
              <th scope="col" className="px-2 py-1 text-start font-semibold">
                {nodeLabel ?? t("actions.targets.node")}
              </th>
            ) : null}
            <th scope="col" className="px-2 py-1 text-start font-semibold">
              {noteLabel ?? t("actions.targets.note")}
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr
              // The same name can be on two nodes, and a list can repeat a row.
              // biome-ignore lint/suspicious/noArrayIndexKey: a static list with no identity beyond its place
              key={i}
              data-skipped={r.skipped || undefined}
              className={cn("border-t", r.skipped && "text-muted-foreground")}
            >
              <td className="px-2 py-1 font-mono text-xs">
                <TruncatedText mono className="max-w-56">
                  {r.name}
                </TruncatedText>
              </td>
              {swarm ? (
                <td className="px-2 py-1">
                  <TruncatedText className="max-w-40">{r.node ?? ""}</TruncatedText>
                </td>
              ) : null}
              <td className="px-2 py-1 text-xs">{r.note ?? ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
