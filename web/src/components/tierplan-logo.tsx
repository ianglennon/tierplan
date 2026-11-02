// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import iconDark from "@/assets/brand/tierplan-icon-dark.svg";
import iconLight from "@/assets/brand/tierplan-icon-light.svg";
import wordmarkDark from "@/assets/brand/tierplan-wordmark-dark.svg";
import wordmarkLight from "@/assets/brand/tierplan-wordmark-light.svg";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The artwork's own proportions (its viewBox), so the images reserve their space before they
// load and nothing shifts.
const SIZES = {
  wordmark: { width: 351, height: 64 },
  icon: { width: 64, height: 64 },
} as const;

interface TierPlanLogoProps {
  variant: "wordmark" | "icon";
  // True where the page already says TierPlan in text: the logo is then hidden from assistive
  // technology so it is not read twice.
  decorative?: boolean;
  // Sizes the logo by its height (h-7, size-6, ...); the artwork fills it.
  className?: string;
}

// TierPlanLogo is the wordmark or the square icon. It holds two images, dark ink for light mode
// and light ink for dark mode, and the dark: variant shows one of them by the dark class on
// <html> (theme-init.js sets it before first paint, lib/theme.ts keeps it), so no script runs
// and nothing flashes. The wrapper is the one thing with a name; the images are decorative.
export function TierPlanLogo({ variant, decorative = false, className }: TierPlanLogoProps) {
  const t = useT();
  const { width, height } = SIZES[variant];
  const light = variant === "wordmark" ? wordmarkLight : iconLight;
  const dark = variant === "wordmark" ? wordmarkDark : iconDark;
  const a11y = decorative
    ? ({ "aria-hidden": true } as const)
    : ({ role: "img", "aria-label": t("common.brand.wordmark") } as const);
  return (
    <span className={cn("inline-block shrink-0", className)} {...a11y}>
      <img
        src={light}
        alt=""
        aria-hidden="true"
        width={width}
        height={height}
        className="block h-full w-auto dark:hidden"
      />
      <img
        src={dark}
        alt=""
        aria-hidden="true"
        width={width}
        height={height}
        className="hidden h-full w-auto dark:block"
      />
    </span>
  );
}
