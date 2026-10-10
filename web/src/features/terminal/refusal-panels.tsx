// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type FormEvent, useEffect, useRef, useState } from "react";

import type { ContainerRow } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { useImmediateAction } from "@/features/containers/row-actions";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// The reasons a shell cannot be opened, each with a panel of its own in the Shell tab
// (UI-SPEC "Shell", D-34, D-35, D-36). The tab itself never hides.
export type Refusal = "notRunning" | "readOnly" | "ownContainer" | "noShell" | "agentTooOld";

interface RefusalPanelProps {
  kind: Refusal;
  envId: string;
  // The container, for the Start button of the Not running panel. Without it the panel
  // says what is wrong and offers nothing.
  row?: ContainerRow;
  // Opens the shell with this command, for the No shell panel.
  onOpenCommand: (command: string) => void;
  // True while that open is under way.
  opening: boolean;
}

// NotRunning: a shell needs a running container; an Operator may start it from here.
function NotRunning({ envId, row }: { envId: string; row?: ContainerRow }) {
  const t = useT();
  return (
    <Card className="max-w-[480px]">
      <CardHeader>
        <CardTitle>
          <h2>{t("terminal.notRunning.heading")}</h2>
        </CardTitle>
        <CardDescription>{t("terminal.notRunning.body")}</CardDescription>
      </CardHeader>
      {row ? (
        <CardContent>
          <StartButton envId={envId} row={row} />
        </CardContent>
      ) : null}
    </Card>
  );
}

function StartButton({ envId, row }: { envId: string; row: ContainerRow }) {
  const t = useT();
  const needs = useNeeds("containers.control", envId);
  const { run, busy } = useImmediateAction(envId, row);
  return (
    <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
      <Button
        type="button"
        className="max-md:h-11"
        disabled={busy}
        aria-busy={busy}
        onClick={() => {
          if (needs.allowed) void run("start");
        }}
      >
        {busy ? (
          <>
            <Spinner aria-hidden="true" />
            {t("terminal.notRunning.starting")}
          </>
        ) : (
          t("terminal.notRunning.start")
        )}
      </Button>
    </NeedsTooltip>
  );
}

// NoShell: the image has neither bash nor sh where Bayplan looks. The custom command
// field is shown and focused, so a path to the image's own shell can be entered.
function NoShell({
  envId,
  onOpenCommand,
  opening,
}: {
  envId: string;
  onOpenCommand: (command: string) => void;
  opening: boolean;
}) {
  const t = useT();
  const needs = useNeeds("containers.exec", envId);
  const [command, setCommand] = useState("");
  const field = useRef<HTMLInputElement>(null);
  useEffect(() => {
    field.current?.focus();
  }, []);

  function submit(event: FormEvent) {
    event.preventDefault();
    const words = command.trim();
    if (needs.allowed && words !== "") onOpenCommand(words);
  }

  return (
    <Card className="max-w-[480px]">
      <CardHeader>
        <CardTitle>
          <h2>{t("terminal.refusal.noShell.heading")}</h2>
        </CardTitle>
        <CardDescription>{t("terminal.refusal.noShell.body")}</CardDescription>
      </CardHeader>
      <CardContent>
        <form noValidate onSubmit={submit} className="flex flex-col gap-3">
          <Field>
            <FieldLabel htmlFor="shell-custom-command">
              {t("terminal.refusal.noShell.command.label")}
            </FieldLabel>
            <Input
              id="shell-custom-command"
              ref={field}
              value={command}
              onChange={(e) => setCommand(e.target.value)}
              className="h-11 font-mono md:h-8"
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              maxLength={4096}
              aria-describedby="shell-custom-command-helper"
            />
            <FieldDescription id="shell-custom-command-helper">
              {t("terminal.refusal.noShell.command.helper")}
            </FieldDescription>
          </Field>
          <div>
            <NeedsTooltip reason={needs.allowed ? undefined : needs.reason}>
              <Button
                type="submit"
                className="max-md:h-11"
                disabled={opening || command.trim() === ""}
                aria-busy={opening}
              >
                {opening ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("terminal.launcher.opening")}
                  </>
                ) : (
                  t("terminal.refusal.noShell.open")
                )}
              </Button>
            </NeedsTooltip>
          </div>
        </form>
      </CardContent>
    </Card>
  );
}

// RefusalPanel is the panel of one reason a shell cannot be opened: not running (with
// Start container), no shell found (with the custom command field), a read-only host, one
// of Bayplan's own containers, or an agent too old to run an exec.
export function RefusalPanel({ kind, envId, row, onOpenCommand, opening }: RefusalPanelProps) {
  const t = useT();
  switch (kind) {
    case "notRunning":
      return <NotRunning envId={envId} row={row} />;
    case "noShell":
      return <NoShell envId={envId} onOpenCommand={onOpenCommand} opening={opening} />;
    case "agentTooOld":
      return <AgentUpdateNotice envId={envId} />;
    case "readOnly":
      return (
        <Card className="max-w-[480px]">
          <CardHeader>
            <CardTitle>
              <h2>{t("terminal.refusal.readOnly.heading")}</h2>
            </CardTitle>
            <CardDescription>{t("terminal.refusal.readOnly.body")}</CardDescription>
          </CardHeader>
        </Card>
      );
    case "ownContainer":
      return (
        <Card className="max-w-[480px]">
          <CardHeader>
            <CardDescription className="text-foreground">
              {t("terminal.refusal.ownContainer")}
            </CardDescription>
          </CardHeader>
        </Card>
      );
  }
}
