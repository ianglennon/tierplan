// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { LockedNote } from "@/features/https/locked-note";
import { RemoveTokenDialog } from "@/features/https/remove-token-dialog";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The id of Replace token, so the DNS-01 suggestion can move focus to it.
export const REPLACE_ID = "acme-dns-replace";
// The id of the token lock note, which the Test token button is described by.
export const TOKEN_LOCK_ID = "acme-dns-token-lock";

interface TokenSavedRowProps {
  // Draws the Replace token button, which swaps the empty field in. Left out, the row has no action.
  onReplace?: () => void;
  // True under an HTTP-01 draft: the token is kept for DNS-01 and the second line says so.
  kept?: boolean;
  // What is saved as the validation method, not what the draft shows (J3). Remove token is offered
  // only when it is HTTP-01: with DNS-01 saved, removing the token would stop every renewal.
  savedChallenge: "http-01" | "dns-01";
  // How many test records are left over at Cloudflare, for the dialog's extra paragraph.
  leftOverCount: number;
  // False when the section is disabled: the row explains, and offers nothing to press.
  canRemove: boolean;
}

// TokenSavedRow is what the page shows of a saved token (CHAL-01, D-05): that there is one, and nothing
// of its value. The token is write-only, so there is no mask, no last characters and no way to read it.
export function TokenSavedRow({
  onReplace,
  kept = false,
  savedChallenge,
  leftOverCount,
  canRemove,
}: TokenSavedRowProps) {
  const t = useT();
  const [removing, setRemoving] = useState(false);
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
      <div className="flex flex-wrap gap-2 max-md:w-full">
        {onReplace ? (
          <Button
            id={REPLACE_ID}
            type="button"
            variant="outline"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            onClick={onReplace}
          >
            {t("https.dns.token.replace")}
          </Button>
        ) : null}
        {savedChallenge === "http-01" && canRemove ? (
          <Button
            type="button"
            variant="destructive"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            onClick={() => setRemoving(true)}
          >
            {t("https.dns.remove.action")}
          </Button>
        ) : null}
      </div>
      {savedChallenge === "dns-01" ? (
        <p className="w-full text-xs text-muted-foreground">{t("https.dns.remove.unavailable")}</p>
      ) : null}
      <RemoveTokenDialog open={removing} onOpenChange={setRemoving} leftOverCount={leftOverCount} />
    </div>
  );
}

interface TokenLockBlockProps {
  // The variable that supplies the token: TIERPLAN_CLOUDFLARE_API_TOKEN or its _FILE form, as the API says.
  varName: string;
  // Whether a token is saved in the database beneath the lock. Only that is known; no token is ever sent
  // to the page, so there is no value here to show.
  saved: boolean;
}

// TokenLockBlock stands in for the write-only field and the Token saved row while a variable supplies
// the token (D-03, D-05, UI-SPEC "Token lock"): who sets it, and whether a token is saved underneath. It
// has no input, no Replace token and no Remove token, and no slot for a value.
export function TokenLockBlock({ varName, saved }: TokenLockBlockProps) {
  const t = useT();
  return (
    <div data-testid="token-locked">
      <LockedNote
        id={TOKEN_LOCK_ID}
        lock={{ var: varName, stored: saved ? "saved" : "none" }}
        storedLabel={t(saved ? "https.lock.tokenSaved" : "https.lock.tokenNone")}
      />
    </div>
  );
}
