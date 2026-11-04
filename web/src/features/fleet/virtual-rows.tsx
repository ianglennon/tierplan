// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useVirtualizer } from "@tanstack/react-virtual";
import type { ReactNode } from "react";

// A row is 36px (44px under 768px for touch). Past 100 rows a fleet table draws only the rows in
// view, inside max-h-[70svh] (UI-SPEC "Fleet tables").
export const FLEET_VIRTUALISE_OVER = 100;
const ROW_PX = 36;
const OVERSCAN = 10;

interface VirtualRowsProps<T> {
  rows: readonly T[];
  // The scrolling box that holds the table.
  scroller: HTMLDivElement | null;
  // The columns the table spans, for the two spacer rows.
  columns: number;
  rowKey: (row: T) => string;
  renderRow: (row: T) => ReactNode;
}

// VirtualRows draws the rows in view between two spacer rows that hold the height of the rest, so
// the table stays a native table and a long list stays responsive.
export function VirtualRows<T>({
  rows,
  scroller,
  columns,
  rowKey,
  renderRow,
}: VirtualRowsProps<T>) {
  const virtualizer = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroller,
    estimateSize: () => ROW_PX,
    overscan: OVERSCAN,
    getItemKey: (index) => {
      const row = rows[index];
      return row ? rowKey(row) : index;
    },
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
          <td colSpan={columns} />
        </tr>
      ) : null}
      {inView.map((v) => {
        const row = rows[v.index];
        return row ? renderRow(row) : null;
      })}
      {after > 0 ? (
        <tr style={{ height: after }}>
          <td colSpan={columns} />
        </tr>
      ) : null}
    </>
  );
}
