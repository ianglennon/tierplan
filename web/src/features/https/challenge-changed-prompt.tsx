// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Info } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useT } from "@/i18n/use-t";

interface ChallengeChangedPromptProps {
  // The saved validation method the next order uses.
  method: string;
  // renew when Renew now is offered (a certificate is served), issue when only Issue certificate is.
  variant: "renew" | "issue";
}

// ChallengeChangedPrompt says that saved validation settings have not been used by an order yet (D-03).
// It is a note with no button and no dismiss: the Renew now or Issue certificate button is the next
// element below it, and the existing reason line explains the buttons when they are disabled.
export function ChallengeChangedPrompt({ method, variant }: ChallengeChangedPromptProps) {
  const t = useT();
  return (
    <Alert role="note">
      <Info aria-hidden="true" />
      <AlertTitle>{t("https.validation.changed.title")}</AlertTitle>
      <AlertDescription>
        {t(`https.validation.changed.${variant}`, { method: method.toUpperCase() })}
      </AlertDescription>
    </Alert>
  );
}
