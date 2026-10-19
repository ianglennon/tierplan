// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Plus } from "lucide-react";
import { type FormEvent, useId, useMemo, useState } from "react";

import type { StackVariable } from "@/api/stacks";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { type ResolveState, VariableRow } from "@/features/stack-editor/variable-row";
import type { WorkingVar } from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";

// The rule for a variable name, the server's: letters, digits and underscore, not starting with a
// digit (stacks.CheckVars). It is the browser's copy so a bad name is caught as it is typed.
const NAME_RULE = /^[A-Za-z_][A-Za-z0-9_]*$/;
const MAX_NAME = 128;

// The rows shown while the draft loads.
const SKELETON_ROWS = 6;

export interface VariablesTableProps {
  // The table of the working copy.
  vars: readonly WorkingVar[];
  // What the server resolved for the working copy at its last check, or null before the first.
  resolved: readonly StackVariable[] | null;
  // The working copy is still loading.
  loading?: boolean;
  // The last check got no answer.
  failed: boolean;
  readOnly: boolean;
  // The caller holds secrets.reveal on the environment: a hidden secret gets a Show button.
  canReveal?: boolean;
  // Fetches the saved value of a secret variable. A plain call, never a query: the answer is held
  // in the row for a minute and nowhere else.
  reveal?: (name: string) => Promise<string>;
  // The names whose saved value the server holds hidden, as the draft was last loaded or saved.
  savedHidden?: ReadonlySet<string>;
  // Open a file of the stack at a line: the links of "Used in n places" call it.
  onOpen?: (file: string, line: number) => void;
  onAdd: (name: string) => void;
  onReset?: (name: string) => void;
  onSet: (name: string, value: string) => void;
  onSecret: (name: string, secret: boolean) => void;
  onRemove: (name: string) => void;
}

interface Row {
  name: string;
  variable?: WorkingVar;
  resolved?: StackVariable;
}

// rowsOf is the union of the variables the files refer to (the server's list, detected without any
// action of the person's) and the variables of the table, by name.
function rowsOf(vars: readonly WorkingVar[], resolved: readonly StackVariable[] | null): Row[] {
  const rows = new Map<string, Row>();
  for (const r of resolved ?? []) rows.set(r.name, { name: r.name, resolved: r });
  for (const v of vars) rows.set(v.name, { ...rows.get(v.name), name: v.name, variable: v });
  return [...rows.values()].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

// VariablesTable is the Variables pane (STK-04, D-01, D-03): one row for every variable the
// compose file refers to and every variable the person added, with the value typed for it, the
// Secret switch and the value a deploy will use, which the server computes. A variable the files
// use has a read-only name; one they do not carries "Unused" and can be removed.
export function VariablesTable({
  vars,
  resolved,
  loading = false,
  failed,
  readOnly,
  canReveal = false,
  reveal,
  savedHidden,
  onAdd,
  onReset,
  onSet,
  onSecret,
  onRemove,
}: VariablesTableProps) {
  const t = useT();
  const formId = useId();
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const rows = useMemo(() => rowsOf(vars, resolved), [vars, resolved]);
  const state: ResolveState = failed ? "failed" : resolved ? "ok" : "pending";

  function submit(e: FormEvent) {
    e.preventDefault();
    const next = name.trim();
    if (next.length > MAX_NAME || !NAME_RULE.test(next)) {
      setError(t("stackVars.add.errors.name"));
      return;
    }
    if (rows.some((r) => r.name === next)) {
      setError(t("stackVars.add.errors.taken"));
      return;
    }
    onAdd(next);
    setName("");
    setError(null);
    setAdding(false);
  }

  function cancel() {
    setName("");
    setError(null);
    setAdding(false);
  }

  return (
    <div className="flex flex-col gap-3">
      {readOnly ? null : (
        <div className="flex flex-col gap-2">
          <div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={loading || adding}
              onClick={() => setAdding(true)}
            >
              <Plus aria-hidden="true" />
              {t("stackVars.add.button")}
            </Button>
          </div>
          {adding ? (
            <form onSubmit={submit} className="flex flex-col gap-1" noValidate>
              <div className="flex flex-wrap items-center gap-2">
                <Input
                  id={`${formId}-name`}
                  autoFocus
                  aria-label={t("stackVars.add.label")}
                  aria-invalid={error ? true : undefined}
                  aria-describedby={error ? `${formId}-error` : undefined}
                  value={name}
                  onChange={(e) => {
                    setName(e.target.value);
                    setError(null);
                  }}
                  autoComplete="off"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                  className="w-64 font-mono max-md:h-11 md:h-8"
                />
                <Button type="submit" size="sm" variant="outline" className="max-md:h-11">
                  {t("stackVars.add.submit")}
                </Button>
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  className="max-md:h-11"
                  onClick={cancel}
                >
                  {t("stackVars.add.cancel")}
                </Button>
              </div>
              {error ? (
                <p id={`${formId}-error`} className="text-xs text-destructive">
                  {error}
                </p>
              ) : null}
            </form>
          ) : null}
        </div>
      )}
      <Table>
        <TableCaption className="mt-0 mb-3 caption-top text-left text-sm text-muted-foreground">
          {t("stackVars.caption")}
        </TableCaption>
        <TableHeader>
          <TableRow>
            <TableHead scope="col">{t("stackVars.columns.name")}</TableHead>
            <TableHead scope="col">{t("stackVars.columns.value")}</TableHead>
            <TableHead scope="col">
              <Tooltip>
                <TooltipTrigger asChild>
                  <span
                    // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
                    tabIndex={0}
                    className="rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                  >
                    {t("stackVars.columns.secret")}
                  </span>
                </TooltipTrigger>
                <TooltipContent className="max-w-sm">{t("stackVars.secretHelp")}</TooltipContent>
              </Tooltip>
            </TableHead>
            <TableHead scope="col">{t("stackVars.columns.resolved")}</TableHead>
            <TableHead scope="col">
              <span className="sr-only">{t("stackVars.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {loading
            ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
                <TableRow key={i} data-testid="variable-skeleton-row" className="h-12">
                  <TableCell colSpan={5} className="py-2">
                    <Skeleton className="h-8 w-full" />
                  </TableCell>
                </TableRow>
              ))
            : rows.map((r) => (
                <VariableRow
                  key={r.name}
                  name={r.name}
                  variable={r.variable}
                  resolved={r.resolved}
                  resolveState={state}
                  unused={resolved !== null && (r.resolved ? r.resolved.unused : true)}
                  readOnly={readOnly}
                  canReveal={canReveal}
                  reveal={reveal}
                  savedHidden={savedHidden?.has(r.name) ?? false}
                  onReset={onReset ? () => onReset(r.name) : undefined}
                  onSet={(value) => onSet(r.name, value)}
                  onSecret={(secret) => onSecret(r.name, secret)}
                  onRemove={() => onRemove(r.name)}
                  onClear={() => onRemove(r.name)}
                />
              ))}
        </TableBody>
      </Table>
      {!loading && rows.length === 0 ? (
        <div className="rounded-lg border p-4">
          <p className="font-medium">{t("stackVars.empty.title")}</p>
          <p className="text-sm text-muted-foreground">{t("stackVars.empty.body")}</p>
        </div>
      ) : null}
    </div>
  );
}
