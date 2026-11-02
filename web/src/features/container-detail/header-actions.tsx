// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronDown } from "lucide-react";
import { Fragment, type ReactNode, useRef, useState } from "react";

import type { ContainerDetail } from "@/api/container-detail";
import type { ContainerRow } from "@/api/resources";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ActionDialog, type DialogVerb } from "@/features/containers/action-dialog";
import { eligibleFor, ownKind } from "@/features/containers/eligibility";
import { type ImmediateAction, useImmediateAction } from "@/features/containers/row-actions";
import { MuteMenuItem } from "@/features/restart-loop/mute-menu-item";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// rowOf is the container as the list's actions know it, so the same eligibility
// rules, dialogs and requests serve the page and the list. The fields the actions
// do not read are left empty.
export function rowOf(d: ContainerDetail): ContainerRow {
  return {
    node: d.node ?? "",
    nodeId: d.nodeId,
    id: d.id,
    name: d.name,
    state: d.state,
    status: "",
    image: d.image,
    created: d.created,
    ports: [],
    isSelf: d.isSelf,
    selfKind: d.selfKind,
    health: d.health,
    readOnly: d.readOnly,
    project: d.project,
    stack: d.stack,
    tierplanStackId: d.tierplanStackId,
    service: d.service,
  };
}

// What one header control does: run at once, or ask first in a dialog.
type Choice = { kind: "run"; action: ImmediateAction } | { kind: "ask"; verb: DialogVerb };

interface Entry {
  id: string;
  label: string;
  choice: Choice;
  remove?: boolean;
  destructive?: boolean;
}

interface HeaderActionsProps {
  envId: string;
  detail: ContainerDetail;
}

// HeaderActions are the buttons in a container page's header, derived from its
// state (D-07): the way forward first, then the rest under More. Start is the one
// accent button, and only while the container is not running. Everything goes
// through the list's ActionDialog and requests, so an action on one of TierPlan's
// own containers asks for its name here as it does there (D-11). The buttons are
// off while a request is out, and a caller who may not act sees them unavailable
// with the reason; the server decides either way.
export function HeaderActions({ envId, detail }: HeaderActionsProps) {
  const t = useT();
  const row = rowOf(detail);
  const needs = useNeeds("containers.control", envId);
  const removeNeeds = useNeeds("containers.remove", envId);
  const { run, busy } = useImmediateAction(envId, row);
  const [dialogVerb, setDialogVerb] = useState<DialogVerb>("stop");
  const [dialogOpen, setDialogOpen] = useState(false);
  const more = useRef<HTMLButtonElement>(null);
  // Set when an item of the menu was chosen, so the menu does not hand focus back to
  // its button before the dialog has opened; the button takes it when the dialog
  // closes, since the item that opened the dialog is gone.
  const chose = useRef(false);
  const [fromMenu, setFromMenu] = useState(false);

  const can = (verb: Parameters<typeof eligibleFor>[0]) => eligibleFor(verb, row);
  const own = ownKind(row) !== "";
  const running = detail.state === "running";

  const label = (verb: string) => t(`actions.menu.${verb}`);
  const restart: Entry = {
    id: "restart",
    label: label("restart"),
    choice: own ? { kind: "ask", verb: "restart" } : { kind: "run", action: "restart" },
  };
  const primary: Entry[] = [];
  const secondary: Entry[] = [];
  if (can("start")) {
    primary.push({
      id: "start",
      label: t("containerdetail.actions.start"),
      choice: { kind: "run", action: "start" },
    });
  }
  if (can("unpause")) {
    primary.push({
      id: "unpause",
      label: label("unpause"),
      choice: { kind: "run", action: "unpause" },
    });
  }
  if (can("stop"))
    primary.push({ id: "stop", label: label("stop"), choice: { kind: "ask", verb: "stop" } });
  if (can("restart")) (running ? primary : secondary).push(restart);
  if (can("pause")) {
    secondary.unshift({
      id: "pause",
      label: label("pause"),
      choice: own ? { kind: "ask", verb: "pause" } : { kind: "run", action: "pause" },
    });
  }
  if (can("kill")) {
    secondary.push({
      id: "kill",
      label: label("kill"),
      choice: { kind: "ask", verb: "kill" },
      destructive: true,
    });
  }
  if (can("remove")) {
    secondary.push({
      id: "remove",
      label: label("remove"),
      choice: { kind: "ask", verb: "remove" },
      remove: true,
      destructive: true,
    });
  }
  if (primary.length === 0 && secondary.length === 0) return null;

  // Unavailable: the reason when the caller's role is short or the host is
  // read-only, and just unavailable while the role is not yet known.
  const readOnlyReason = detail.readOnly ? t("actions.readOnlyHost") : undefined;
  const reasonFor = (e: Entry) => (e.remove ? removeNeeds : needs).reason ?? readOnlyReason;
  const unavailable = (e: Entry) => !(e.remove ? removeNeeds : needs).allowed || detail.readOnly;

  function choose(e: Entry, viaMenu: boolean) {
    if (busy) return;
    if (e.choice.kind === "run") {
      void run(e.choice.action);
      return;
    }
    setFromMenu(viaMenu);
    setDialogVerb(e.choice.verb);
    setDialogOpen(true);
  }

  const wrap = (e: Entry, control: ReactNode) => (
    <NeedsTooltip key={e.id} reason={unavailable(e) ? reasonFor(e) : undefined}>
      {control}
    </NeedsTooltip>
  );

  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        {primary.map((e) =>
          wrap(
            e,
            <Button
              type="button"
              variant={e.id === "start" ? "default" : "outline"}
              className="max-md:h-11"
              disabled={busy || (unavailable(e) && !reasonFor(e))}
              onClick={() => choose(e, false)}
            >
              {e.label}
            </Button>,
          ),
        )}
        {secondary.length > 0 ? (
          <DropdownMenu>
            <DropdownMenuTrigger asChild>
              <Button
                ref={more}
                type="button"
                variant="outline"
                className="max-md:h-11"
                disabled={busy}
                aria-label={t("containerdetail.actions.more")}
              >
                {t("containerdetail.actions.moreText")}
                <ChevronDown aria-hidden="true" data-icon="inline-end" />
              </Button>
            </DropdownMenuTrigger>
            <DropdownMenuContent
              align="end"
              onCloseAutoFocus={(event) => {
                if (!chose.current) return;
                chose.current = false;
                event.preventDefault();
              }}
            >
              {detail.restartLoop || detail.muted ? (
                <>
                  <MuteMenuItem
                    kind="container"
                    envId={envId}
                    nodeId={detail.nodeId}
                    containerId={detail.id}
                    name={detail.name}
                    muted={detail.muted}
                    needs={needs}
                  />
                  <DropdownMenuSeparator />
                </>
              ) : null}
              {secondary.map((e, i) => (
                <Fragment key={e.id}>
                  {e.destructive && !secondary[i - 1]?.destructive && i > 0 ? (
                    <DropdownMenuSeparator />
                  ) : null}
                  {wrap(
                    e,
                    <DropdownMenuItem
                      className="max-md:min-h-11"
                      variant={e.destructive ? "destructive" : "default"}
                      disabled={unavailable(e) && !reasonFor(e)}
                      onSelect={() => {
                        if (busy) return;
                        chose.current = true;
                        choose(e, true);
                      }}
                    >
                      {e.label}
                    </DropdownMenuItem>,
                  )}
                </Fragment>
              ))}
            </DropdownMenuContent>
          </DropdownMenu>
        ) : null}
      </div>
      <ActionDialog
        envId={envId}
        verb={dialogVerb}
        rows={[row]}
        open={dialogOpen}
        onClose={() => setDialogOpen(false)}
        onClosed={() => {
          if (fromMenu) more.current?.focus();
          setFromMenu(false);
        }}
      />
    </>
  );
}
