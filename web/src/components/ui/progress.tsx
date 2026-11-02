// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
// SPDX-FileCopyrightText: 2023 shadcn. Derived from shadcn/ui (https://github.com/shadcn-ui/ui), used under the MIT licence; see the third-party notices.
import { Progress as ProgressPrimitive } from "radix-ui";
import type * as React from "react";

import { cn } from "@/lib/utils";

// Progress is the bar of a pull (Phase 5 UI-SPEC). The fill is the foreground colour at
// 70% on the muted track: it is neither the accent nor a status colour, and the text beside
// it carries the exact amount. A value that is null or left out is indeterminate: the
// primitive then leaves aria-valuenow off, and the fill is not drawn.
function Progress({
  className,
  value,
  ...props
}: React.ComponentProps<typeof ProgressPrimitive.Root>) {
  return (
    <ProgressPrimitive.Root
      data-slot="progress"
      value={value}
      className={cn(
        "relative flex h-2 w-full items-center overflow-x-hidden rounded-full bg-muted",
        className,
      )}
      {...props}
    >
      <ProgressPrimitive.Indicator
        data-slot="progress-indicator"
        className="size-full flex-1 bg-foreground/70 transition-transform motion-reduce:transition-none"
        style={{ transform: `translateX(-${100 - (value || 0)}%)` }}
      />
    </ProgressPrimitive.Root>
  );
}

export { Progress };
