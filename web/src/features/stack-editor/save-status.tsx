// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { RelativeTime } from "@/components/relative-time";
import { useT } from "@/i18n/use-t";

interface SaveStatusProps {
  // The working copy differs from the saved draft.
  dirty: boolean;
  // When and by whom the draft was saved last, or empty when no draft has been saved.
  savedAt?: string;
  savedBy?: string;
}

// SaveStatus is the left side of the editor footer (UI-SPEC "Editor"): what the person has is not
// saved (a warning dot beside the words, never colour alone), when and by whom the shared draft was
// saved, or that there is no draft yet. It is a polite live region, so the change from unsaved to
// saved is announced once. On a narrow screen the footer may give the status its own row at the top
// of the bar, and the words can break there rather than spill out (G-09-10).
export function SaveStatus({ dirty, savedAt, savedBy }: SaveStatusProps) {
  const t = useT();
  return (
    <p role="status" className="flex min-w-0 items-center gap-2 text-sm">
      {dirty ? (
        <>
          <span aria-hidden="true" className="size-2 shrink-0 rounded-full bg-status-warning" />
          <span className="min-w-0 break-words">{t("stackEditor.status.unsaved")}</span>
        </>
      ) : savedAt ? (
        <span className="min-w-0 break-words text-muted-foreground">
          {t("stackEditor.status.saved")} <RelativeTime value={savedAt} />{" "}
          {t("stackEditor.status.savedBy", { user: savedBy })}
        </span>
      ) : (
        <span className="text-muted-foreground">{t("stackEditor.status.none")}</span>
      )}
    </p>
  );
}
