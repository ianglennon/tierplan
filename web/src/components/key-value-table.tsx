// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

import { MaskedValue } from "@/components/masked-value";
import { Table, TableBody, TableCell, TableRow } from "@/components/ui/table";
import { cn } from "@/lib/utils";

// One row of a KeyValueTable. A row with `masked` set shows a MaskedValue in place
// of its value: with `value` and `canReveal` the holder of secrets.reveal can show
// it, and without them there is nothing to show, because the server withheld it.
export type KeyValueRow =
  | { key: string; value: ReactNode; masked?: false }
  | { key: string; masked: true; value?: undefined; canReveal?: false }
  | { key: string; masked: true; value: string; canReveal: true };

interface KeyValueTableProps {
  rows: readonly KeyValueRow[];
  // The words shown, and read out as the table's name, when there are no rows.
  empty: string;
  // The table's accessible name.
  label: string;
  // Keys and values in mono, for labels, variables and other machine text. Off for
  // a table of plain words.
  mono?: boolean;
}

// KeyValueTable is a two-column table of names and values: the labels and the
// environment variables of a container, and the rows of a state card. Long text
// wraps at any character rather than widening the card. Everything it draws is
// text: nothing Docker supplies is read as markup.
export function KeyValueTable({ rows, empty, label, mono = true }: KeyValueTableProps) {
  if (rows.length === 0) {
    return <p className="text-sm text-muted-foreground">{empty}</p>;
  }
  const cell = cn("align-top whitespace-normal break-all", mono && "font-mono text-xs");
  return (
    <Table aria-label={label}>
      <TableBody>
        {rows.map((row) => (
          <TableRow key={row.key}>
            <TableCell className={cn(cell, "w-2/5 text-muted-foreground", !mono && "break-words")}>
              {row.key}
            </TableCell>
            <TableCell className={cn(cell, !mono && "break-words")}>
              {row.masked === true ? (
                row.canReveal === true ? (
                  <MaskedValue name={row.key} canReveal value={row.value} />
                ) : (
                  <MaskedValue name={row.key} />
                )
              ) : (
                row.value
              )}
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}
