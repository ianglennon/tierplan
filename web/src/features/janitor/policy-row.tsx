// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import type { ReactNode } from "react";

import { NeedsTooltip } from "@/components/needs-tooltip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";

// The ids a row gives its parts, so the control inside it can point at the helper and the error.
export const rowIds = (id: string) => ({
  title: `${id}-title`,
  helper: `${id}-helper`,
  error: `${id}-error`,
});

interface PolicyRowProps {
  // The id the row's parts are named from (see rowIds).
  id: string;
  label: string;
  // The id of the control the label names. A row whose control is a switch or a group of controls
  // has its own labels and leaves this out.
  labelFor?: string;
  // Absent when the control says it itself (the schedule's helper depends on its preset).
  helper?: ReactNode;
  // True when the environment sets the field itself; false when it follows the instance default.
  // Absent on a row that has no default to follow (the instance defaults on Settings, General), and
  // then the row says neither Default nor Overridden and offers no Reset.
  overridden?: boolean;
  // The name of the field in "Reset {field} to default".
  field?: string;
  onReset?: () => void;
  // Why the person may not change the row (NeedsTooltip); absent when they may.
  locked?: string;
  // What is wrong with the value, drawn under the control.
  error?: string;
  // The control, with its own label where the row's label does not name it.
  children: ReactNode;
}

// PolicyRow is one setting of the policy card: its label and helper on the left and the control on
// the right (stacked under 768px), and under the control where its value comes from. A field shows
// "Default" while it follows the instance default and "Overridden", with a Reset button, once this
// environment sets it itself (D-04). There is no separate override switch: changing the control is
// what overrides it. "Default" and "Overridden" are words, never a colour alone.
export function PolicyRow({
  id,
  label,
  labelFor,
  helper,
  overridden,
  field,
  onReset,
  locked,
  error,
  children,
}: PolicyRowProps) {
  const t = useT();
  const ids = rowIds(id);
  return (
    <fieldset
      aria-labelledby={ids.title}
      className="grid min-w-0 gap-3 border-b py-4 first:pt-0 md:grid-cols-[minmax(0,1fr)_minmax(0,28rem)] md:gap-6"
    >
      <div className="flex min-w-0 flex-col gap-1">
        {labelFor ? (
          <Label id={ids.title} htmlFor={labelFor} className="text-sm font-medium">
            {label}
          </Label>
        ) : (
          <span id={ids.title} className="text-sm font-medium">
            {label}
          </span>
        )}
        {helper ? (
          <p id={ids.helper} className="text-sm text-muted-foreground">
            {helper}
          </p>
        ) : null}
      </div>
      <div className="flex min-w-0 flex-col gap-2">
        {children}
        {error ? (
          <p id={ids.error} role="alert" className="text-sm text-destructive">
            {error}
          </p>
        ) : null}
        {overridden === undefined ? null : (
          <div className="flex min-h-6 flex-wrap items-center gap-2">
            {overridden ? (
              <>
                <Badge variant="outline">{t("janitorPolicy.state.overridden")}</Badge>
                <NeedsTooltip reason={locked}>
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    className="max-md:h-11"
                    aria-label={t("janitorPolicy.state.resetField", { field: field ?? label })}
                    onClick={onReset}
                  >
                    {t("janitorPolicy.state.reset")}
                  </Button>
                </NeedsTooltip>
              </>
            ) : (
              <Tooltip>
                <TooltipTrigger asChild>
                  {/* Focusable, so the tooltip opens from the keyboard too. */}
                  <Badge variant="outline" tabIndex={0}>
                    {t("janitorPolicy.state.default")}
                  </Badge>
                </TooltipTrigger>
                <TooltipContent className="max-w-80">
                  {t("janitorPolicy.state.defaultTooltip")}
                </TooltipContent>
              </Tooltip>
            )}
          </div>
        )}
      </div>
    </fieldset>
  );
}
