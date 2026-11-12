// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Lock } from "lucide-react";
import type { ReactElement } from "react";

import { rich } from "@/features/https/sentence";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A lock as the status carries it: the variable that fixes the setting, and the value kept in the
// database beneath it (absent when nothing is stored, never set for a secret).
export interface LockInfo {
  var: string;
  stored?: string;
}

// LockedIcon is the lock beside a locked field, named for a screen reader as the settings pages name
// it (setup.instance.url.lockedIcon, "Locked").
export function LockedIcon({ className }: { className?: string }) {
  const t = useT();
  return (
    <Lock
      role="img"
      aria-label={t("setup.instance.url.lockedIcon")}
      className={cn("size-4 shrink-0", className)}
    />
  );
}

// What line 1 says. set is the common sentence; accepted is the terms checkbox (acceptance is not a
// stored setting, so it has no line 2); order and stop are the reasons a locked action gives.
export type LockVariant = "set" | "accepted" | "order" | "stop";

interface LockedNoteProps {
  // The id the control or button is described by (aria-describedby): it covers both lines.
  id: string;
  lock: LockInfo;
  variant?: LockVariant;
  // The Lock icon leads line 1. False where the control carries the icon itself (an input addon).
  showIcon?: boolean;
  // False where the field has no stored counterpart to show (the terms, a summary of an account that
  // already exists).
  showStored?: boolean;
  // A free-text stored value (an email, a profile name): drawn in mono and broken anywhere.
  mono?: boolean;
  // How an enum stored value reads, when it is not the raw value (not for free text).
  storedLabel?: string;
}

// LockedNote is the two lock sentences every locked field shows (UI-SPEC "Lock rows"): who sets it,
// and the value kept underneath that comes back if the variable is removed. The variable's name is
// always the API's, never written here, so the label names the variable in use. An empty stored value
// reads "none": the second line is never blank.
export function LockedNote({
  id,
  lock,
  variant = "set",
  showIcon = true,
  showStored,
  mono = false,
  storedLabel,
}: LockedNoteProps) {
  const t = useT();
  const withStored = showStored ?? variant !== "accepted";
  const hasStored = lock.stored !== undefined && lock.stored !== "";
  let value: string | ReactElement;
  if (!hasStored) value = <span>{t("https.lock.none")}</span>;
  else if (mono) value = lock.stored ?? "";
  else value = <span>{storedLabel ?? lock.stored}</span>;

  return (
    <div id={id} className="flex min-w-0 flex-col gap-1">
      <p className="flex items-start gap-2 text-sm text-muted-foreground">
        {showIcon ? <LockedIcon className="mt-0.5" /> : null}
        <span className="min-w-0 break-words">
          {rich(t, `https.lock.${variant}`, { var: lock.var })}
        </span>
      </p>
      {withStored ? (
        <p className={cn("min-w-0 break-words text-xs text-muted-foreground", showIcon && "pl-6")}>
          {rich(t, "https.lock.stored", { value, var: lock.var })}
        </p>
      ) : null}
    </div>
  );
}
