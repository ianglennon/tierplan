// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useSetContainerRestartLoopMute, useSetRestartLoopMute } from "@/api/restart-loops";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { DropdownMenuItem } from "@/components/ui/dropdown-menu";
import type { Needs } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface CommonProps {
  envId: string;
  name: string;
  // True while the restart-loop alerts are muted: the item then unmutes.
  muted: boolean;
  // Whether the signed-in user may control services (or containers) on the environment, and if
  // not why not.
  needs: Needs;
  // Called as the item is chosen, so a row menu can keep focus where it was.
  onChosen?: () => void;
}

type MuteMenuItemProps = CommonProps &
  (
    | { kind?: "service"; serviceId: string }
    // A container is muted by the node it is on and its name; the route takes the ID or the name.
    | { kind: "container"; nodeId: string; containerId: string }
  );

// MuteMenuItem is "Mute restart-loop alerts" / "Unmute restart-loop alerts" in a service's or a
// standalone container's menu (RES-13, D-16). It needs no confirmation: a mute is audited and
// undone from the same menu. A caller below Operator sees it unavailable with the reason (D-19);
// the server decides either way. The toast and the refresh belong to the mutation, since this
// menu is gone before the answer.
export function MuteMenuItem(props: MuteMenuItemProps) {
  const { envId, name, muted, needs, onChosen } = props;
  const t = useT();
  const muteService = useSetRestartLoopMute(envId);
  const muteContainer = useSetContainerRestartLoopMute(envId);
  return (
    <NeedsTooltip reason={needs.reason}>
      <DropdownMenuItem
        className="max-md:min-h-11"
        disabled={!needs.allowed && !needs.reason}
        onSelect={() => {
          if (!needs.allowed) return;
          onChosen?.();
          if (props.kind === "container") {
            muteContainer.mutate({
              nodeId: props.nodeId,
              containerId: props.containerId,
              name,
              muted: !muted,
            });
          } else {
            muteService.mutate({ serviceId: props.serviceId, name, muted: !muted });
          }
        }}
      >
        {t(muted ? "restartLoop.mute.unmute" : "restartLoop.mute.mute")}
      </DropdownMenuItem>
    </NeedsTooltip>
  );
}
