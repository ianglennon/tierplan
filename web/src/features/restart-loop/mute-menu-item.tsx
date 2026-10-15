// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useSetRestartLoopMute } from "@/api/restart-loops";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import type { Needs } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface MuteMenuItemProps {
  envId: string;
  serviceId: string;
  name: string;
  // True while the service's restart-loop alerts are muted: the item then unmutes.
  muted: boolean;
  // Whether the signed-in user may control services on the environment, and if not why not.
  needs: Needs;
  // Called as the item is chosen, so a row menu can keep focus where it was.
  onChosen?: () => void;
}

// MuteMenuItem is "Mute restart-loop alerts" / "Unmute restart-loop alerts" in a service's menu
// (RES-13, D-16). It needs no confirmation: a mute is audited and undone from the same menu. A
// caller below Operator sees it unavailable with the reason (D-19); the server decides either way.
// The toast and the refresh belong to the mutation, since this menu is gone before the answer.
export function MuteMenuItem({
  envId,
  serviceId,
  name,
  muted,
  needs,
  onChosen,
}: MuteMenuItemProps) {
  const t = useT();
  const mute = useSetRestartLoopMute(envId);
  return (
    <NeedsTooltip reason={needs.reason}>
      <DropdownMenuItem
        className="max-md:min-h-11"
        disabled={!needs.allowed && !needs.reason}
        onSelect={() => {
          if (!needs.allowed) return;
          onChosen?.();
          mute.mutate({ serviceId, name, muted: !muted });
        }}
      >
        {t(muted ? "restartLoop.mute.unmute" : "restartLoop.mute.mute")}
      </DropdownMenuItem>
    </NeedsTooltip>
  );
}
