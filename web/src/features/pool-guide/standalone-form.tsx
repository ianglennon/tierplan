// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Plus, X } from "lucide-react";
import { useId, useRef } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  MAX_POOLS,
  type PoolRow,
  validatePoolRows,
} from "@/features/address-space/address-pools-card";
import { CommandBlock } from "@/features/pool-guide/command-block";
import { daemonJsonSnippet } from "@/features/pool-guide/generate";
import { useDebounced } from "@/features/pool-guide/use-debounced";
import { useT } from "@/i18n/use-t";

// A pool row being typed. The key is only for the list, so a removed row does not move the text of
// the one under it.
export interface PoolDraft extends PoolRow {
  key: number;
}

// What the tab opens with: one row to type over.
export const INITIAL_POOLS: PoolDraft[] = [{ key: 0, base: "10.20.0.0/16", size: "24" }];

// snippetOf is the daemon.json text for rows that passed the checks, or null when the generator
// refuses one all the same: a line is never made from a row it would not vouch for.
function snippetOf(rows: readonly PoolRow[]): string | null {
  try {
    return daemonJsonSnippet(rows);
  } catch {
    return null;
  }
}

interface StandaloneFormProps {
  envId: string;
  rows: PoolDraft[];
  onChange: (rows: PoolDraft[]) => void;
  // True when this environment is a standalone host, whose settings can hold the same pools.
  standalone: boolean;
}

// StandaloneForm is the Standalone host tab (D-23 part 4): the pools as rows, the daemon.json
// snippet they make and the restart note. A row that fails shows its line and the snippet waits;
// the snippet is text to merge in by hand and is never sent anywhere.
export function StandaloneForm({ envId, rows, onChange, standalone }: StandaloneFormProps) {
  const t = useT();
  const id = useId();
  const addButton = useRef<HTMLButtonElement>(null);
  const nextKey = useRef(Math.max(0, ...rows.map((r) => r.key)) + 1);
  const settled = useDebounced(rows);
  const errors = validatePoolRows(settled, t);
  const complete = settled.length > 0 && settled.every((r) => r.base.trim() && r.size.trim());
  const snippet = complete && !errors.some((e) => e.base || e.size) ? snippetOf(settled) : null;

  function setField(key: number, field: keyof PoolRow, value: string) {
    onChange(rows.map((r) => (r.key === key ? { ...r, [field]: value } : r)));
  }

  return (
    <div className="flex flex-col gap-4">
      <p className="text-sm text-muted-foreground">{t("poolGuide.standalone.intro")}</p>
      <div className="grid gap-8 lg:grid-cols-[2fr_3fr]">
        <div className="flex flex-col gap-3">
          {rows.length > 0 ? (
            <Table>
              <TableCaption className="sr-only">
                {t("poolGuide.standalone.tableLabel")}
              </TableCaption>
              <TableHeader>
                <TableRow>
                  <TableHead scope="col" className="min-w-40">
                    {t("envsettings.addressPools.base")}
                  </TableHead>
                  <TableHead scope="col" className="w-24">
                    {t("envsettings.addressPools.size")}
                  </TableHead>
                  <TableHead scope="col" className="w-11">
                    <span className="sr-only">{t("envsettings.addressPools.actions")}</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {rows.map((row, i) => {
                  const n = i + 1;
                  // The row as the snippet last saw it: a line shows once the person has typed in
                  // the field, and not on a row they have not touched.
                  const own = errors[settled.findIndex((s) => s.key === row.key)];
                  const baseLine = row.base.trim() ? own?.base : undefined;
                  const sizeLine = row.size.trim() ? own?.size : undefined;
                  return (
                    <TableRow key={row.key} className="align-top">
                      <TableCell>
                        <Input
                          aria-label={t("envsettings.addressPools.baseAria", { n })}
                          placeholder={t("envsettings.addressPools.basePlaceholder")}
                          autoComplete="off"
                          autoCapitalize="off"
                          spellCheck={false}
                          value={row.base}
                          aria-invalid={baseLine ? true : undefined}
                          aria-describedby={baseLine ? `${id}-base-${row.key}` : undefined}
                          className="font-mono max-md:h-11"
                          onChange={(e) => setField(row.key, "base", e.target.value)}
                        />
                        {baseLine ? (
                          <p
                            id={`${id}-base-${row.key}`}
                            role="alert"
                            className="mt-1 text-xs text-destructive"
                          >
                            {baseLine}
                          </p>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        <Input
                          type="number"
                          inputMode="numeric"
                          aria-label={t("envsettings.addressPools.sizeAria", { n })}
                          placeholder={t("envsettings.addressPools.sizePlaceholder")}
                          value={row.size}
                          aria-invalid={sizeLine ? true : undefined}
                          aria-describedby={sizeLine ? `${id}-size-${row.key}` : undefined}
                          className="max-md:h-11"
                          onChange={(e) => setField(row.key, "size", e.target.value)}
                        />
                        {sizeLine ? (
                          <p
                            id={`${id}-size-${row.key}`}
                            role="alert"
                            className="mt-1 text-xs text-destructive"
                          >
                            {sizeLine}
                          </p>
                        ) : null}
                      </TableCell>
                      <TableCell>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon"
                          className="size-11 md:size-8"
                          aria-label={t("envsettings.addressPools.remove", { n })}
                          onClick={() => {
                            onChange(rows.filter((r) => r.key !== row.key));
                            // The row that had focus is gone: Add pool is where the person carries on.
                            requestAnimationFrame(() => addButton.current?.focus());
                          }}
                        >
                          <X aria-hidden="true" />
                        </Button>
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          ) : null}
          <p className="text-xs text-muted-foreground">
            {t("envsettings.addressPools.sizeHelper")}
          </p>
          <Button
            ref={addButton}
            type="button"
            variant="outline"
            className="w-fit max-md:h-11 max-md:w-full"
            aria-disabled={rows.length >= MAX_POOLS || undefined}
            onClick={() => {
              if (rows.length < MAX_POOLS) {
                onChange([...rows, { key: nextKey.current++, base: "", size: "" }]);
              }
            }}
          >
            <Plus aria-hidden="true" />
            {t("envsettings.addressPools.add")}
          </Button>
        </div>
        <div className="flex min-w-0 flex-col gap-4">
          {snippet ? (
            <>
              <CommandBlock caption={t("poolGuide.standalone.snippetCaption")} command={snippet} />
              <p className="text-sm text-muted-foreground">{t("poolGuide.standalone.restart")}</p>
            </>
          ) : (
            <p role="status" className="text-sm text-muted-foreground">
              {t("poolGuide.standalone.incomplete")}
            </p>
          )}
          {standalone ? (
            <div className="flex flex-col items-start gap-2">
              <p className="text-sm">{t("poolGuide.standalone.followUp")}</p>
              <Button asChild variant="outline" className="max-md:h-11">
                <Link to="/settings/environments/$envId" params={{ envId }} hash="address-pools">
                  {t("poolGuide.standalone.openSettings")}
                </Link>
              </Button>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  );
}
