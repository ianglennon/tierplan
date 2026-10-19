// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CircleX, Trash2, X } from "lucide-react";

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
  onSet: (value: string) => void;
  onSecret: (secret: boolean) => void;
  onRemove: () => void;
  onClear: () => void;
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
  onSet,
  onSecret,
  onRemove,
  onClear,
}: VariableRowProps) {
  const t = useT();
  const secret = variable?.secret ?? false;
  const hidden = variable?.hidden ?? false;
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
        <Input
          aria-label={t("stackVars.row.valueOf", { name })}
          type={secret ? "password" : "text"}
          autoComplete={secret ? "new-password" : "off"}
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          disabled={readOnly}
          value={variable?.value ?? ""}
          placeholder={placeholder}
          onChange={(e) => onSet(e.target.value)}
          className="min-w-40 font-mono max-md:h-11 md:h-8"
        />
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
