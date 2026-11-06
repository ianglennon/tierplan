// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Search } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Kbd } from "@/components/ui/kbd";
import { useSearchPalette } from "@/features/search/palette-context";
import { useT } from "@/i18n/use-t";
import { isMacPlatform, shortcutLabel } from "@/lib/env-nav";
import { cn } from "@/lib/utils";

// SearchTrigger is the header's way into the search palette (D-18): a 224px button styled as an
// input from 768px, with the icon, the word "Search" and the shortcut; a 44px icon button under
// 768px. It is one button, so there is one control by this name whatever the width. It opens the
// palette EnvPalette keeps, and closing the palette puts focus back here.
export function SearchTrigger() {
  const t = useT();
  const { setOpen, returnTo } = useSearchPalette();
  return (
    <Button
      type="button"
      variant="ghost"
      aria-label={t("search.trigger.label")}
      aria-keyshortcuts={isMacPlatform() ? "Meta+K" : "Control+K"}
      onClick={(event) => {
        returnTo.current = event.currentTarget;
        setOpen(true);
      }}
      className={cn(
        "gap-2 max-md:size-11 md:h-8 md:w-56 md:justify-start md:border-input md:bg-input/30 md:px-2.5 md:font-normal md:text-muted-foreground",
      )}
    >
      <Search aria-hidden="true" />
      <span className="max-md:hidden">{t("search.trigger.text")}</span>
      <Kbd className="ms-auto max-md:hidden" aria-hidden="true">
        {shortcutLabel()}
      </Kbd>
    </Button>
  );
}
