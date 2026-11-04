// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";

import {
  type IgnoreTarget,
  type UpdateInfo,
  type UpdateWorkload,
  useSkipUpdate,
  useUnignoreUpdates,
  useUnskipUpdate,
} from "@/api/updates";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import { useEnvName } from "@/features/resources/use-env-name";
import { CheckNowItem } from "@/features/updates/check-now-item";
import { canBeNamed, imageRefOf, imageTextOf } from "@/features/updates/image-ref";
import { useUpdateGate } from "@/features/updates/update-gate";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

// The states whose image can be checked, so that an ignore means something: a pinned image never
// changes and an image from no registry has nothing to compare with.
const IGNORABLE: ReadonlySet<UpdateInfo["state"]> = new Set([
  "update_available",
  "updating",
  "busy",
  "queued",
  "up_to_date",
  "check_stale",
  "check_failed",
  "rolled_back",
  "rollback_failed",
  "not_checked",
  "skipped",
]);

// ignoreTargetOf is what a "Stop ignoring updates" takes back, read from the ignore the server
// reports on the row: the image the row follows, or the row itself.
function ignoreTargetOf(workload: UpdateWorkload, update: UpdateInfo): IgnoreTarget {
  if (update.ignore?.scope === "image" && canBeNamed(update)) {
    return { scope: "image", image: imageRefOf(update), text: imageTextOf(update) };
  }
  return { scope: "workload", kind: workload.kind, name: workload.name, text: workload.name };
}

interface UpdateMenuItemsProps {
  envId: string;
  // The service or container the row is, as the skip and ignore routes name it.
  workload: UpdateWorkload;
  // What the server says about the row's image. Absent, only Check now is offered.
  update?: UpdateInfo;
  // Opens the ignore dialog. The dialog belongs to the row, not to this item: a menu's content is
  // gone as soon as an item is chosen.
  onIgnore: () => void;
  // Opens the update confirm. Absent where the row cannot be updated here (TierPlan's own agent), and
  // then no Update… is shown. A service or container of a stack TierPlan manages is updated as a new
  // revision of its stack, and the confirm takes the person to the stack's console. Like the ignore
  // dialog, the confirm belongs to the row.
  onUpdate?: () => void;
  // For a container: the Compose project it belongs to and the swarm service it is a task of. The
  // server refuses a one-click update of a container of a project TierPlan does not manage
  // (the item is "Adopt to update…" and goes to that project's adopt flow, D-21) and of a task
  // container (the item is unavailable and names the service to update instead).
  project?: string;
  service?: string;
  // Called as an item that opens something is chosen, so a row menu can keep focus where it was.
  onChosen?: () => void;
}

// UpdateMenuItems is the update part of a row menu on the services list, the containers list and a
// stack's services (D-05, D-15, D-16): "Update…" where the row can be updated here (it opens the confirm;
// it is unavailable with the reason when it cannot work yet, D-10; after a rollback it reads "Update
// anyway…"), "Skip this version" for an update on offer, "Stop skipping" for a
// skipped one, "Ignore updates…" unless the compose label already does, "Stop ignoring updates"
// for an ignored one, and "Check now" always. Skip and ignore need Env Admin (updates.manage); below
// it they are unavailable with the reason (D-10), and the server decides either way. An ignore set
// by the tierplan.updates=ignore label on the object cannot be removed here: the item is unavailable
// and says how. Skip needs no confirm. The toasts, Undo included, belong to the mutations, since this
// menu is gone before the answer.
export function UpdateMenuItems({
  envId,
  workload,
  update,
  onIgnore,
  onUpdate,
  project,
  service,
  onChosen,
}: UpdateMenuItemsProps) {
  const t = useT();
  const needs = useNeeds("updates.manage", envId);
  const envName = useEnvName(envId);
  const skip = useSkipUpdate(envId);
  const unskip = useUnskipUpdate(envId);
  const unignore = useUnignoreUpdates(envId);
  const reason = needs.reason
    ? t("updates.menu.needs", { needs: needs.reason, environment: envName })
    : undefined;
  // An item below the permission is shown, not hidden: aria-disabled with the reason, or
  // disabled outright while the role is not yet known.
  const blocked = !needs.allowed;
  const unavailable = blocked && !reason;

  const isContainer = workload.kind === "container";
  const gate = useUpdateGate(
    envId,
    update,
    isContainer ? { kind: "container", service: service || workload.name } : undefined,
  );
  // The server's refusal of a container: compose_unmanaged is sent to the adopt flow here, and
  // task_container is worded by the gate as the reason Update… is unavailable.
  const canAdopt =
    isContainer && update?.refusal === "compose_unmanaged" && gate.shown && !!project;
  const canUpdate = onUpdate !== undefined && gate.shown && !gate.adopt;

  const state = update?.state;
  const labelled = update?.ignore?.scope === "label";
  // A version that failed before is retried on purpose: the item says so, and the confirm that opens is the
  // Update anyway form.
  const retry = state === "rolled_back" || state === "rollback_failed";
  const canSkip = state === "update_available";
  const canUnskip = state === "skipped";
  const canUnignore = state === "ignored";
  const canIgnore = state !== undefined && IGNORABLE.has(state);

  return (
    <>
      {canAdopt ? (
        <NeedsTooltip reason={gate.reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={gate.unavailable}
            asChild={!gate.reason && !gate.unavailable}
            onSelect={() => {
              if (gate.reason || gate.unavailable) return;
              onChosen?.();
            }}
          >
            {gate.reason || gate.unavailable ? (
              t("updates.stack.action.adopt")
            ) : (
              <Link to="/env/$envId/unmanaged-stacks/$name/adopt" params={{ envId, name: project }}>
                {t("updates.stack.action.adopt")}
              </Link>
            )}
          </DropdownMenuItem>
        </NeedsTooltip>
      ) : null}
      {canUpdate ? (
        <NeedsTooltip reason={gate.reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={gate.unavailable}
            onSelect={() => {
              if (gate.reason || gate.unavailable) return;
              onChosen?.();
              onUpdate?.();
            }}
          >
            {t(retry ? "updates.menu.updateAnyway" : "updates.menu.update")}
          </DropdownMenuItem>
        </NeedsTooltip>
      ) : null}
      {canSkip ? (
        <NeedsTooltip reason={reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={unavailable}
            onSelect={() => {
              if (blocked) return;
              skip.mutate({ ...workload, digest: update?.remoteDigest });
            }}
          >
            {t("updates.menu.skip")}
          </DropdownMenuItem>
        </NeedsTooltip>
      ) : null}
      {canUnskip ? (
        <NeedsTooltip reason={reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={unavailable}
            onSelect={() => {
              if (blocked) return;
              unskip.mutate(workload);
            }}
          >
            {t("updates.menu.unskip")}
          </DropdownMenuItem>
        </NeedsTooltip>
      ) : null}
      {canIgnore ? (
        <NeedsTooltip reason={reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={unavailable}
            onSelect={() => {
              if (blocked) return;
              onChosen?.();
              onIgnore();
            }}
          >
            {t("updates.menu.ignore")}
          </DropdownMenuItem>
        </NeedsTooltip>
      ) : null}
      {canUnignore ? (
        <NeedsTooltip reason={labelled ? t("updates.menu.labelIgnored") : reason}>
          <DropdownMenuItem
            className="max-md:min-h-11"
            disabled={unavailable && !labelled}
            onSelect={() => {
              if (blocked || labelled || !update) return;
              unignore.mutate(ignoreTargetOf(workload, update));
            }}
          >
            {t("updates.menu.unignore")}
          </DropdownMenuItem>
        </NeedsTooltip>
      ) : null}
      <CheckNowItem envId={envId} />
    </>
  );
}
