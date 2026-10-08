// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import {
  cloneElement,
  isValidElement,
  type MouseEvent,
  type ReactElement,
  type ReactNode,
  useId,
} from "react";

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { toastInfo } from "@/lib/toast";
import { cn } from "@/lib/utils";

interface NeedsTooltipProps {
  // Why the action is unavailable. Absent means it is available: the child is
  // rendered as it is, with no tooltip.
  reason?: string;
  // The one control that is unavailable: a button, a switch or a menu item.
  children: ReactNode;
}

interface ChildProps {
  className?: string;
  onClick?: (event: MouseEvent<HTMLElement>) => void;
}

// NeedsTooltip wraps an action the user may see but not use (D-19). The control
// gets aria-disabled="true" and not the disabled attribute, so it stays in the
// tab order and the reason opens on keyboard focus as well as on hover. It looks
// disabled, ignores activation, and has the reason as its aria-describedby text.
// Because touch has no hover, a tap or click shows the same reason as a toast. A
// reason is never the only sign that the control is unavailable: it also looks
// it.
export function NeedsTooltip({ reason, children }: NeedsTooltipProps) {
  const reasonId = useId();
  if (!reason || !isValidElement(children)) return <>{children}</>;
  const child = children as ReactElement<ChildProps>;

  function activate(event: MouseEvent<HTMLElement>) {
    event.preventDefault();
    event.stopPropagation();
    toastInfo(reason ?? "");
  }

  return (
    <>
      <Tooltip>
        <TooltipTrigger asChild>
          {cloneElement(child, {
            "aria-disabled": true,
            "aria-describedby": reasonId,
            onClick: activate,
            className: cn(child.props.className, "cursor-not-allowed opacity-50"),
          } as ChildProps)}
        </TooltipTrigger>
        <TooltipContent>{reason}</TooltipContent>
      </Tooltip>
      <span id={reasonId} className="sr-only">
        {reason}
      </span>
    </>
  );
}
