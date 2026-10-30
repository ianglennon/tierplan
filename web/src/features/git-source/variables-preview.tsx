// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { CircleX, Trash2, TriangleAlert, X } from "lucide-react";
import { type ReactNode, useRef } from "react";

import type { GitStackVariable } from "@/api/git";
import { MaskedValue } from "@/components/masked-value";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SettingsCard } from "@/features/environments/settings-card";
import { HIDDEN } from "@/features/spec-editor/diff";
import { useT } from "@/i18n/use-t";

// Four dots, as the secret-like value of a committed .env shows in Resolved (UI-SPEC "Variables
// table"). They are always these four, whatever the value, so its length is not given away.
const DOTS = "••••";

// A name longer than this is cut off with a tooltip that carries the whole of it.
const LONG_NAME = 24;

// One variable of the table the person fills in: the value they typed and whether it is secret.
// It lives in the page's state, is sent with the check and the create, and is never shown back.
export interface TableVar {
  name: string;
  value: string;
  secret: boolean;
}

export interface VariablesPreviewProps {
  // Every variable the compose files use and every one sent, as the last check resolved them; null
  // before the first check has answered.
  vars: readonly GitStackVariable[] | null;
  value: readonly TableVar[];
  onChange: (next: TableVar[]) => void;
  // At least one compose file is chosen.
  enabled: boolean;
  // A check is waiting or on its way.
  checking: boolean;
}

function ResolvedCell({ v }: { v: GitStackVariable }) {
  const t = useT();
  let shown: ReactNode;
  if (v.required) {
    shown = (
      <span className="inline-flex items-center gap-1 text-status-offline-text">
        <CircleX aria-hidden="true" className="size-4 shrink-0" />
        {t("stackVars.row.required")}
      </span>
    );
  } else if (v.value === HIDDEN && v.secretLike && v.source === "repo-env") {
    shown = (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
            tabIndex={0}
            className="w-fit rounded-sm font-mono text-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {DOTS}
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm">{t("gitSource.vars.committedTip")}</TooltipContent>
      </Tooltip>
    );
  } else if (v.value === HIDDEN) {
    shown = <MaskedValue name={v.name} />;
  } else if (v.value === "") {
    shown = <span className="text-muted-foreground">{t("stackVars.resolved.empty")}</span>;
  } else {
    shown = <span className="font-mono text-sm break-all">{v.value}</span>;
  }
  return (
    <div className="flex min-w-0 flex-col">
      {shown}
      <span className="text-xs text-muted-foreground">
        {t(`gitSource.vars.source.${v.source}`)}
      </span>
    </div>
  );
}

interface RowProps {
  v: GitStackVariable;
  entry: TableVar | undefined;
  setValue: (value: string) => void;
  setSecret: (secret: boolean) => void;
  clear: () => void;
  setSecretValue: () => void;
  register: (el: HTMLInputElement | null) => void;
}

// GitVariableRow is one row of the Variables card: the row of the editor's table with the place the
// value comes from under Resolved, and under it, for a secret-like value committed in the .env,
// the warning that offers Set secret value.
function GitVariableRow({
  v,
  entry,
  setValue,
  setSecret,
  clear,
  setSecretValue,
  register,
}: RowProps) {
  const t = useT();
  const secret = entry?.secret ?? false;
  const typed = entry?.value ?? "";
  const places = v.places?.length ?? 0;
  // The value the field falls back to: the .env value, else the default, else Required.
  const placeholder =
    v.source === "repo-env" && v.value !== HIDDEN && v.value !== ""
      ? v.value
      : v.hasDefault && v.default !== HIDDEN
        ? v.default
        : v.unused || v.secretLike
          ? undefined
          : t("stackVars.row.required");
  const warn = v.secretLike && v.source === "repo-env" && typed === "";
  const nameText = <span className="block max-w-56 truncate font-mono text-sm">{v.name}</span>;
  return (
    <>
      <TableRow data-testid={`git-variable-row-${v.name}`} className="h-12">
        <TableCell className="py-2 align-middle">
          {v.name.length > LONG_NAME ? (
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
              <TooltipContent className="max-w-sm break-all">{v.name}</TooltipContent>
            </Tooltip>
          ) : (
            nameText
          )}
          {v.unused ? (
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
            ref={register}
            aria-label={t("stackVars.row.valueOf", { name: v.name })}
            type={secret ? "password" : "text"}
            autoComplete={secret ? "new-password" : "off"}
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            value={typed}
            placeholder={placeholder}
            onChange={(e) => setValue(e.target.value)}
            className="min-w-40 font-mono max-md:h-11 md:h-8"
          />
        </TableCell>
        <TableCell className="py-2 align-middle">
          <Switch
            size="sm"
            checked={secret}
            aria-label={t("stackVars.row.secretOf", { name: v.name })}
            onCheckedChange={setSecret}
          />
        </TableCell>
        <TableCell className="min-w-40 max-w-72 py-2 align-middle">
          <ResolvedCell v={v} />
        </TableCell>
        <TableCell className="py-2 text-right align-middle">
          {v.unused ? (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="max-md:size-11"
              aria-label={t("stackVars.row.remove", { name: v.name })}
              onClick={clear}
            >
              <Trash2 aria-hidden="true" />
            </Button>
          ) : typed !== "" ? (
            <Button
              type="button"
              variant="ghost"
              size="icon"
              className="max-md:size-11"
              aria-label={t("stackVars.row.clear", { name: v.name })}
              onClick={clear}
            >
              <X aria-hidden="true" />
            </Button>
          ) : null}
        </TableCell>
      </TableRow>
      {warn ? (
        <TableRow data-testid={`git-variable-warning-${v.name}`}>
          <TableCell colSpan={5} className="py-2 whitespace-normal">
            <div className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm text-status-warning-text">
              <TriangleAlert aria-hidden="true" className="size-4 shrink-0" />
              <span className="min-w-0 flex-1 break-words">
                {t("gitSource.vars.secretLike", { name: v.name })}
              </span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={setSecretValue}
              >
                {t("gitSource.vars.setSecret")}
              </Button>
            </div>
          </TableCell>
        </TableRow>
      ) : null}
    </>
  );
}

// VariablesPreview is Card 3 of the From git page (UI-SPEC "Variables table", D-07, GIT-08): a row
// for every ${VAR} the compose files use, with the value typed for it, the Secret switch and the
// value a deploy would use, which the server resolves, with where that value comes from. The table
// of values is the page's state; this only draws the server's answer and reports what is typed.
// A secret-like value committed in the repository's .env is never shown, and a typed secret is
// never given back.
export function VariablesPreview({
  vars,
  value,
  onChange,
  enabled,
  checking,
}: VariablesPreviewProps) {
  const t = useT();
  const inputs = useRef(new Map<string, HTMLInputElement>());

  function entryOf(name: string): TableVar | undefined {
    return value.find((v) => v.name === name);
  }

  // A row with neither a value nor the Secret switch on is no row of the table.
  function update(name: string, patch: Partial<TableVar>) {
    const have = entryOf(name);
    const next: TableVar = {
      name,
      value: have?.value ?? "",
      secret: have?.secret ?? false,
      ...patch,
    };
    const rest = value.filter((v) => v.name !== name);
    if (next.value === "" && !next.secret) onChange(rest);
    else if (have) onChange(value.map((v) => (v.name === name ? next : v)));
    else onChange([...rest, next]);
  }

  let body: ReactNode;
  if (!enabled) {
    body = <p className="text-sm text-muted-foreground">{t("gitSource.vars.first")}</p>;
  } else if (vars === null) {
    body = checking ? (
      <div className="flex flex-col gap-2">
        <p className="text-sm text-muted-foreground">{t("gitSource.check.checking")}</p>
        <Skeleton className="h-8 w-full" aria-hidden="true" />
      </div>
    ) : (
      <p className="text-sm text-muted-foreground">{t("gitSource.vars.waiting")}</p>
    );
  } else if (vars.length === 0) {
    body = (
      <div className="rounded-lg border p-4">
        <p className="font-medium">{t("gitSource.vars.empty.title")}</p>
        <p className="text-sm text-muted-foreground">{t("gitSource.vars.empty.body")}</p>
      </div>
    );
  } else {
    body = (
      <div data-testid="git-variables-scroll" className="max-h-96 overflow-y-auto">
        <Table>
          <TableHeader className="sticky top-0 z-10 bg-card">
            <TableRow>
              <TableHead scope="col">{t("stackVars.columns.name")}</TableHead>
              <TableHead scope="col">{t("stackVars.columns.value")}</TableHead>
              <TableHead scope="col">{t("stackVars.columns.secret")}</TableHead>
              <TableHead scope="col">{t("stackVars.columns.resolved")}</TableHead>
              <TableHead scope="col">
                <span className="sr-only">{t("stackVars.columns.actions")}</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {vars.map((v) => (
              <GitVariableRow
                key={v.name}
                v={v}
                entry={entryOf(v.name)}
                setValue={(next) => update(v.name, { value: next })}
                setSecret={(secret) => update(v.name, { secret })}
                clear={() => onChange(value.filter((e) => e.name !== v.name))}
                setSecretValue={() => {
                  update(v.name, { secret: true });
                  inputs.current.get(v.name)?.focus();
                }}
                register={(el) => {
                  if (el) inputs.current.set(v.name, el);
                  else inputs.current.delete(v.name);
                }}
              />
            ))}
          </TableBody>
        </Table>
      </div>
    );
  }

  return (
    <SettingsCard title={t("gitSource.vars.title")}>
      <p className="text-sm text-muted-foreground">{t("gitSource.vars.body")}</p>
      {body}
    </SettingsCard>
  );
}

// RequiredVariablesLine is the count of variables a deploy needs and has no value for (UI-SPEC
// footer). It sits above the footer and never stops Create: a draft saves with problems, and Review
// is where the deploy is refused.
export function RequiredVariablesLine({ vars }: { vars: readonly GitStackVariable[] | null }) {
  const t = useT();
  const count = (vars ?? []).filter((v) => v.required && !v.unused).length;
  if (count === 0) return null;
  return (
    <p role="status" className="flex items-start gap-2 text-sm text-status-warning-text">
      <TriangleAlert aria-hidden="true" className="mt-0.5 size-4 shrink-0" />
      <span className="min-w-0 break-words">{t("gitSource.vars.required", { count })}</span>
    </p>
  );
}
