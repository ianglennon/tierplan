// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check } from "lucide-react";

import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface TokenSavedRowProps {
  // Draws the Replace token button, which swaps the empty field in. Left out, the row has no action.
  onReplace?: () => void;
  // True under an HTTP-01 draft: the token is kept for DNS-01 and the second line says so.
  kept?: boolean;
}

// TokenSavedRow is what the page shows of a saved token (CHAL-01, D-05): that there is one, and nothing
// of its value. The token is write-only, so there is no mask, no last characters and no way to read it.
export function TokenSavedRow({ onReplace, kept = false }: TokenSavedRowProps) {
  const t = useT();
  return (
    <div className="flex flex-wrap items-start justify-between gap-2" data-testid="token-saved">
      <div className="flex min-w-0 flex-col gap-1">
        <p className="flex items-center gap-2 text-sm font-medium">
          <Check aria-hidden="true" className="size-4 shrink-0 text-status-online-text" />
          {t("https.dns.token.saved")}
        </p>
        <p className="text-xs text-muted-foreground">
          {t(kept ? "https.dns.token.kept" : "https.dns.token.savedNote")}
        </p>
      </div>
      {onReplace ? (
        <Button
          type="button"
          variant="outline"
          className={`${TOUCH_HEIGHT} max-md:w-full`}
          onClick={onReplace}
        >
          {t("https.dns.token.replace")}
        </Button>
      ) : null}
    </div>
  );
}
