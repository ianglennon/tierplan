// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ContainerRow } from "@/api/resources";
import { AgentUpdateNotice } from "@/components/agent-update-notice";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Spinner } from "@/components/ui/spinner";
import { useImmediateAction } from "@/features/containers/row-actions";
import { DEFAULT_CHOICE, type ShellChoice, ShellForm } from "@/features/terminal/shell-launcher";
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
  // Opens the shell as chosen, for the No shell panel.
  onOpen: (choice: ShellChoice) => void;
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

// NoShell: the image has neither bash nor sh where TierPlan looks. The launcher's form is
// shown with Custom command chosen and its field focused, so a path to the image's own
// shell can be entered.
const CUSTOM_CHOICE: ShellChoice = { ...DEFAULT_CHOICE, shell: "custom" };

function NoShell({
  envId,
  onOpen,
  opening,
}: {
  envId: string;
  onOpen: (choice: ShellChoice) => void;
  opening: boolean;
}) {
  const t = useT();
  return (
    <Card className="max-w-[480px]">
      <CardHeader>
        <CardTitle>
          <h2>{t("terminal.refusal.noShell.heading")}</h2>
        </CardTitle>
        <CardDescription>{t("terminal.refusal.noShell.body")}</CardDescription>
      </CardHeader>
      <CardContent>
        <ShellForm
          envId={envId}
          initial={CUSTOM_CHOICE}
          focusCommand
          opening={opening}
          onOpen={onOpen}
        />
      </CardContent>
    </Card>
  );
}

// RefusalPanel is the panel of one reason a shell cannot be opened: not running (with
// Start container), no shell found (with the custom command field), a read-only host, one
// of TierPlan's own containers, or an agent too old to run an exec.
export function RefusalPanel({ kind, envId, row, onOpen, opening }: RefusalPanelProps) {
  const t = useT();
  switch (kind) {
    case "notRunning":
      return <NotRunning envId={envId} row={row} />;
    case "noShell":
      return <NoShell envId={envId} onOpen={onOpen} opening={opening} />;
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
