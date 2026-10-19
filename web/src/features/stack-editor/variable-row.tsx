// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CircleX, Eye, EyeOff, RotateCcw, Trash2, X } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import type { StackVariable } from "@/api/stacks";
import { MaskedValue } from "@/components/masked-value";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { TableCell, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { HIDDEN } from "@/features/spec-editor/diff";
import type { WorkingVar } from "@/features/stack-editor/working-copy";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A name longer than this is cut off with a tooltip that carries the whole of it.
const LONG_NAME = 24;

// How long a revealed value stays in the input before it is hidden again (UI-SPEC "Design System").
export const REVEAL_MS = 60_000;

// Whether the server has said how the variables resolve: not yet, it could not, or it has.
export type ResolveState = "pending" | "failed" | "ok";

export interface VariableRowProps {
  name: string;
  // The variable as the table of the working copy holds it, if it does.
  variable?: WorkingVar;
  // The server's row for this name, if it has answered with one.
  resolved?: StackVariable;
  resolveState: ResolveState;
  // True when the server has answered and its list does not name this variable: nothing refers
  // to it. False until it answers, so a row is not called unused before anyone has looked.
  unused: boolean;
  readOnly: boolean;
  // The caller may show a secret's saved value, and how to fetch it.
  canReveal?: boolean;
  reveal?: (name: string) => Promise<string>;
  // The server holds this variable's value hidden, as the draft was last loaded or saved.
  savedHidden?: boolean;
  onSet: (value: string) => void;
  onSecret: (secret: boolean) => void;
  onRemove: () => void;
  onClear: () => void;
  onReset?: () => void;
}

// ResolvedCell is the value the deploy will use for a variable and where it comes from. It is the
// server's answer, never computed in the browser (UI-SPEC "Variables pane").
function ResolvedCell({
  name,
  resolved,
  state,
}: {
  name: string;
  resolved?: StackVariable;
  state: ResolveState;
}) {
  const t = useT();
  if (!resolved) {
    if (state === "pending") {
      return <Skeleton data-testid="resolved-skeleton" className="h-4 w-24" />;
    }
    return (
      <div className="flex flex-col">
        <span>{t("stackVars.resolved.failedValue")}</span>
        {state === "failed" ? (
          <span className="text-xs text-muted-foreground">{t("stackVars.resolved.failed")}</span>
        ) : null}
      </div>
    );
  }
  return (
    <div className="flex min-w-0 flex-col">
      {resolved.required ? (
        <span className="inline-flex items-center gap-1 text-status-offline-text">
          <CircleX aria-hidden="true" className="size-4 shrink-0" />
          {t("stackVars.row.required")}
        </span>
      ) : resolved.secret && resolved.value === HIDDEN ? (
        <MaskedValue name={name} />
      ) : resolved.value === "" ? (
        <span className="text-muted-foreground">{t("stackVars.resolved.empty")}</span>
      ) : (
        <span className="font-mono text-sm break-all">{resolved.value}</span>
      )}
      <span className="text-xs text-muted-foreground">
        {t(`stackVars.resolved.source.${resolved.source}`)}
      </span>
      {state === "failed" ? (
        <span className="text-xs text-muted-foreground">{t("stackVars.resolved.failed")}</span>
      ) : null}
    </div>
  );
}

// VariableRow is one row of the Variables table: a variable the compose file refers to or the
// table holds. The name of a variable the files use is read-only; its value is typed here and the
// Resolved cell, computed by the server, shows what a deploy will use.
export function VariableRow({
  name,
  variable,
  resolved,
  resolveState,
  unused,
  readOnly,
  canReveal = false,
  reveal,
  savedHidden = false,
  onSet,
  onSecret,
  onRemove,
  onClear,
  onReset,
}: VariableRowProps) {
  const t = useT();
  const helperId = useId();
  const secret = variable?.secret ?? false;
  const hidden = variable?.hidden ?? false;
  // The revealed value lives here and nowhere else: not in the working copy, not in a query, not
  // in storage. It goes when the minute is up, when it is edited, and with the row.
  const [revealed, setRevealed] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  const [revealError, setRevealError] = useState<string | null>(null);
  const [notice, setNotice] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      clearTimeout(timer.current);
    };
  }, []);

  function hide(announce: boolean) {
    clearTimeout(timer.current);
    setRevealed(null);
    if (announce) setNotice(t("stackVars.row.hiddenAgain", { name }));
  }

  // A value the server holds hidden that is no longer hidden in the working copy (typed over, reset,
  // reloaded) is not shown from the reveal any more.
  useEffect(() => {
    if (revealed !== null && !hidden) {
      clearTimeout(timer.current);
      setRevealed(null);
    }
  }, [revealed, hidden]);

  async function toggleReveal() {
    if (revealed !== null) {
      hide(true);
      return;
    }
    if (!reveal || pending) return;
    setPending(true);
    setRevealError(null);
    setNotice("");
    try {
      const value = await reveal(name);
      if (!mounted.current) return;
      setRevealed(value);
      clearTimeout(timer.current);
      timer.current = setTimeout(() => hide(true), REVEAL_MS);
    } catch (e) {
      if (!mounted.current) return;
      const status = (e as { status?: number }).status;
      setRevealError(
        status === 403 ? t("stackVars.row.revealDenied") : t("stackVars.row.revealFailed"),
      );
    } finally {
      if (mounted.current) setPending(false);
    }
  }

  const showButton = canReveal && reveal !== undefined && secret && (hidden || revealed !== null);
  // A hidden secret that was typed over can be put back to what the server holds.
  const resetButton = !readOnly && savedHidden && secret && !hidden && revealed === null && onReset;
  // Secret turned off on a value the server holds hidden: the text is not here to become plain.
  const typeAgain = savedHidden && !secret && variable !== undefined && variable.value === "";
  const places = resolved?.places?.length ?? 0;
  const placeholder = hidden
    ? t("stackVars.row.hiddenPlaceholder")
    : resolved?.hasDefault
      ? resolved.default
      : resolved && !resolved.unused
        ? t("stackVars.row.required")
        : undefined;
  const hasTableValue = variable !== undefined && (variable.value !== "" || variable.hidden);
  const nameText = <span className="block max-w-56 truncate font-mono text-sm">{name}</span>;
  return (
    <TableRow data-testid={`variable-row-${name}`} className="h-12">
      <TableCell className="py-2 align-middle">
        {name.length > LONG_NAME ? (
          <Tooltip>
            <TooltipTrigger asChild>
              <span
                // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
                tabIndex={0}
                className="block rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
              >
                {nameText}
              </span>
            </TooltipTrigger>
            <TooltipContent className="max-w-sm break-all">{name}</TooltipContent>
          </Tooltip>
        ) : (
          nameText
        )}
        {unused ? (
          <Badge variant="outline" className="mt-0.5">
            {t("stackVars.row.unused")}
          </Badge>
        ) : places > 0 ? (
          <span className="block text-xs text-muted-foreground">
            {t("stackVars.row.usedIn", { count: places })}
          </span>
        ) : null}
      </TableCell>
      <TableCell className="py-2 align-middle">
        <div className="flex items-center gap-1">
          <Input
            aria-label={t("stackVars.row.valueOf", { name })}
            aria-describedby={typeAgain ? helperId : undefined}
            type={secret && revealed === null ? "password" : "text"}
            autoComplete={secret ? "new-password" : "off"}
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            disabled={readOnly && revealed === null}
            readOnly={readOnly}
            value={revealed ?? variable?.value ?? ""}
            placeholder={placeholder}
            onChange={(e) => {
              if (revealed !== null) {
                clearTimeout(timer.current);
                setRevealed(null);
              }
              onSet(e.target.value);
            }}
            className="min-w-40 font-mono max-md:h-11 md:h-8"
          />
          {showButton ? (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="shrink-0 max-md:size-11"
              aria-pressed={revealed !== null}
              aria-label={t("stackVars.row.show", { name })}
              disabled={pending}
              onClick={() => void toggleReveal()}
            >
              {revealed !== null ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
            </Button>
          ) : null}
          {resetButton ? (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="shrink-0 max-md:size-11"
              aria-label={t("stackVars.row.reset", { name })}
              onClick={onReset}
            >
              <RotateCcw aria-hidden="true" />
            </Button>
          ) : null}
        </div>
        {typeAgain ? (
          <p id={helperId} className="mt-1 text-xs text-muted-foreground">
            {t("stackVars.row.turnedOff")}
          </p>
        ) : null}
        {revealError ? <p className="mt-1 text-xs text-destructive">{revealError}</p> : null}
        <span role="status" aria-live="polite" className="sr-only">
          {notice}
        </span>
      </TableCell>
      <TableCell className="py-2 align-middle">
        <Switch
          size="sm"
          checked={secret}
          disabled={readOnly}
          aria-label={t("stackVars.row.secretOf", { name })}
          onCheckedChange={onSecret}
        />
      </TableCell>
      <TableCell className={cn("py-2 align-middle", "min-w-40 max-w-72")}>
        <ResolvedCell name={name} resolved={resolved} state={resolveState} />
      </TableCell>
      <TableCell className="py-2 text-right align-middle">
        {readOnly ? null : unused ? (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            aria-label={t("stackVars.row.remove", { name })}
            onClick={onRemove}
          >
            <Trash2 aria-hidden="true" />
          </Button>
        ) : hasTableValue ? (
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="max-md:size-11"
            aria-label={t("stackVars.row.clear", { name })}
            onClick={onClear}
          >
            <X aria-hidden="true" />
          </Button>
        ) : null}
      </TableCell>
    </TableRow>
  );
}
