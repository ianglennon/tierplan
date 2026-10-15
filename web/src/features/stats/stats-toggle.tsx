// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useId } from "react";

import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

interface StatsToggleProps {
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  // What the switch adds and when it streams, for a page where it adds something other than
  // the list columns. The default is the lists'.
  hint?: string;
}

// StatsToggle is the Live stats switch of the services, containers and placement pages
// (plan 06-33, D-31). It is off until it is turned on, and its state lives in the page's
// address (stats=1), so a link opens the same view.
export function StatsToggle({ checked, onCheckedChange, hint }: StatsToggleProps) {
  const t = useT();
  const id = useId();
  return (
    <div className="flex items-center gap-2">
      <Tooltip>
        <TooltipTrigger asChild>
          <div className="flex items-center gap-2">
            <Switch
              id={id}
              checked={checked}
              onCheckedChange={onCheckedChange}
              className="max-md:my-2"
            />
            <Label htmlFor={id} className="text-sm text-muted-foreground">
              {t("stats.list.toggle")}
            </Label>
          </div>
        </TooltipTrigger>
        <TooltipContent>{hint ?? t("stats.list.toggleHint")}</TooltipContent>
      </Tooltip>
    </div>
  );
}
