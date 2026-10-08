// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

interface TruncatedProps {
  text: string;
  className?: string;
}

// Truncated cuts a long name to its column with the whole of it in a Tooltip that
// opens on keyboard focus as well as hover (UI-SPEC "long-text": names inside
// lists truncate with a Tooltip). The tooltip text is a text node, never markup.
export function Truncated({ text, className }: TruncatedProps) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC D-66)
          tabIndex={0}
          className={cn(
            "block max-w-56 truncate rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
            className,
          )}
        >
          {text}
        </span>
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-all">{text}</TooltipContent>
    </Tooltip>
  );
}
