// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

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
  // Called as an item that opens something is chosen, so a row menu can keep focus where it was.
  onChosen?: () => void;
}

// UpdateMenuItems is the update part of a row menu on the services list, the containers list and a
// stack's services (D-15, D-16): "Skip this version" for an update on offer, "Stop skipping" for a
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

  const state = update?.state;
  const labelled = update?.ignore?.scope === "label";
  const canSkip = state === "update_available";
  const canUnskip = state === "skipped";
  const canUnignore = state === "ignored";
  const canIgnore = state !== undefined && IGNORABLE.has(state);

  return (
    <>
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
