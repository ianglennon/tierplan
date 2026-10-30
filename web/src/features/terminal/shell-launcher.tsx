// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronDown } from "lucide-react";
import { type FormEvent, useEffect, useId, useRef, useState } from "react";

import type { ShellOpenError, ShellPreference } from "@/api/shells";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// The shells the launcher offers. Automatic runs bash when the image has it and sh
// otherwise, in one exec; custom runs a command split into words, never through sh -c.
export const SHELL_KINDS = ["auto", "bash", "sh", "ash", "custom"] as const;
export type ShellKind = (typeof SHELL_KINDS)[number];

// What the person chose: the shell, the custom command, the user to run as and the
// working directory. There is no way to ask for environment variables or a privileged
// exec (D-26, D-27).
export interface ShellChoice {
  shell: ShellKind;
  command: string;
  user: string;
  workingDir: string;
}

export const DEFAULT_CHOICE: ShellChoice = { shell: "auto", command: "", user: "", workingDir: "" };

// The caps the server holds the fields to.
const MAX_COMMAND = 4096;
const MAX_USER = 64;
const MAX_DIR = 4096;

function isKind(v: string): v is ShellKind {
  return (SHELL_KINDS as readonly string[]).includes(v);
}

// choiceOf turns the server's remembered preference into a choice.
export function choiceOf(p: ShellPreference | null | undefined): ShellChoice | undefined {
  if (!p) return undefined;
  return {
    shell: isKind(p.shell) ? p.shell : "auto",
    command: p.command,
    user: p.user,
    workingDir: p.workingDir,
  };
}

interface ShellFormProps {
  envId: string;
  // What the form starts from, and moves to when the remembered choice arrives after
  // the form is up, as long as the person has not changed anything yet.
  initial?: ShellChoice;
  // Puts the cursor in the Command field when the form appears (the No shell panel).
  focusCommand?: boolean;
  // True while an open is under way.
  opening: boolean;
  onOpen: (choice: ShellChoice) => void;
}

// ShellForm is the Shell select, the Command field a custom shell needs, the collapsed
// Options (User, Working directory) and the Open shell button (UI-SPEC "Shell").
export function ShellForm({ envId, initial, focusCommand, opening, onOpen }: ShellFormProps) {
  const t = useT();
  const needs = useNeeds("containers.exec", envId);
  const id = useId();
  const [choice, setChoice] = useState<ShellChoice>(initial ?? DEFAULT_CHOICE);
  const [options, setOptions] = useState(Boolean(initial?.user || initial?.workingDir));
  const touched = useRef(false);
  const command = useRef<HTMLInputElement>(null);
  // Set when Custom command was just chosen, so that when the list closes focus goes to
  // the field it revealed and not back to the select.
  const toCommand = useRef(false);

  // The remembered choice arrives from the server after the form is up; it is taken
  // only while nothing has been changed by hand.
  useEffect(() => {
    if (!initial || touched.current) return;
    setChoice(initial);
    if (initial.user || initial.workingDir) setOptions(true);
  }, [initial]);

  useEffect(() => {
    if (focusCommand) command.current?.focus();
  }, [focusCommand]);

  const custom = choice.shell === "custom";
  const incomplete = custom && choice.command.trim() === "";

  function change(patch: Partial<ShellChoice>) {
    touched.current = true;
    setChoice((c) => ({ ...c, ...patch }));
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!needs.allowed || opening || incomplete) return;
    onOpen({
      shell: choice.shell,
      command: custom ? choice.command.trim() : "",
      user: choice.user.trim(),
      workingDir: choice.workingDir.trim(),
    });
  }

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <FieldGroup>
        <Field>
          <FieldLabel htmlFor={`${id}-shell`}>{t("terminal.launcher.shell.label")}</FieldLabel>
          <Select
            value={choice.shell}
            onValueChange={(v) => {
              if (!isKind(v)) return;
              change({ shell: v });
              toCommand.current = v === "custom";
            }}
          >
            <SelectTrigger id={`${id}-shell`} className="h-11 w-full md:h-8">
              <SelectValue />
            </SelectTrigger>
            <SelectContent
              position="popper"
              align="start"
              onCloseAutoFocus={(e) => {
                if (!toCommand.current) return;
                toCommand.current = false;
                e.preventDefault();
                command.current?.focus();
              }}
            >
              {SHELL_KINDS.map((k) => (
                <SelectItem key={k} value={k}>
                  {t(`terminal.launcher.shell.${k}`)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </Field>
        {custom ? (
          <Field>
            <FieldLabel htmlFor={`${id}-command`}>
              {t("terminal.launcher.command.label")}
            </FieldLabel>
            <Input
              id={`${id}-command`}
              ref={command}
              value={choice.command}
              onChange={(e) => change({ command: e.target.value })}
              className="h-11 font-mono md:h-8"
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              maxLength={MAX_COMMAND}
              aria-describedby={`${id}-command-helper`}
            />
            <FieldDescription id={`${id}-command-helper`}>
              {t("terminal.launcher.command.helper")}
            </FieldDescription>
          </Field>
        ) : null}
      </FieldGroup>

      <Collapsible open={options} onOpenChange={setOptions}>
        <CollapsibleTrigger asChild>
          <Button type="button" variant="ghost" size="sm" className="-ml-2 max-md:h-11">
            <ChevronDown
              aria-hidden="true"
              className={options ? "transition-transform" : "-rotate-90 transition-transform"}
            />
            {t("terminal.launcher.options")}
          </Button>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <FieldGroup className="pt-2">
            <Field>
              <FieldLabel htmlFor={`${id}-user`}>{t("terminal.launcher.user.label")}</FieldLabel>
              <Input
                id={`${id}-user`}
                value={choice.user}
                onChange={(e) => change({ user: e.target.value })}
                placeholder={t("terminal.launcher.user.placeholder")}
                className="h-11 font-mono md:h-8"
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                maxLength={MAX_USER}
                aria-describedby={`${id}-user-helper`}
              />
              <FieldDescription id={`${id}-user-helper`}>
                {t("terminal.launcher.user.helper")}
              </FieldDescription>
            </Field>
            <Field>
              <FieldLabel htmlFor={`${id}-dir`}>{t("terminal.launcher.dir.label")}</FieldLabel>
              <Input
                id={`${id}-dir`}
                value={choice.workingDir}
                onChange={(e) => change({ workingDir: e.target.value })}
                placeholder={t("terminal.launcher.dir.placeholder")}
                className="h-11 font-mono md:h-8"
                autoComplete="off"
                autoCapitalize="off"
                autoCorrect="off"
                spellCheck={false}
                maxLength={MAX_DIR}
              />
            </Field>
          </FieldGroup>
        </CollapsibleContent>
      </Collapsible>

      <div>
        <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
          <Button
            type="submit"
            className="max-md:h-11"
            disabled={opening || incomplete}
            aria-busy={opening}
          >
            {opening ? (
              <>
                <Spinner aria-hidden="true" />
                {t("terminal.launcher.opening")}
              </>
            ) : (
              t("terminal.launcher.open")
            )}
          </Button>
        </NeedsTooltip>
      </div>
    </form>
  );
}

interface ShellLauncherProps extends Omit<ShellFormProps, "focusCommand"> {
  // The container's name and, on a swarm, the node, for the words.
  name: string;
  node?: string;
  // Why the last attempt to open a shell failed, when it did and has no panel of its own.
  failure?: ShellOpenError;
  // The key of the line to show for a failure that is not the cap.
  failureCopyKey?: string;
}

// ShellLauncher is the card before a session: what a shell is, the form with the last
// choice for this image preselected, and what went wrong with the last attempt.
export function ShellLauncher({
  name,
  node,
  failure,
  failureCopyKey,
  ...form
}: ShellLauncherProps) {
  const t = useT();
  return (
    <Card className="max-w-[480px]">
      <CardHeader>
        <CardTitle>
          <h2>{t("terminal.launcher.heading", { name })}</h2>
        </CardTitle>
        <CardDescription>
          {node ? t("terminal.launcher.bodyNode", { node }) : t("terminal.launcher.body")}
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-col gap-3">
        {failure?.type === "shell_limit_reached" && failure.limit ? (
          <Alert variant="destructive">
            <AlertDescription>
              {t("terminal.refusal.limit", { count: failure.limit })}
            </AlertDescription>
          </Alert>
        ) : failure && failureCopyKey ? (
          <Alert variant="destructive">
            <AlertDescription>{t(failureCopyKey)}</AlertDescription>
          </Alert>
        ) : null}
        <ShellForm {...form} />
      </CardContent>
    </Card>
  );
}
