// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { GitPullRequestArrow } from "lucide-react";
import { useState } from "react";

import { AcceptLiveError, useAcceptLive } from "@/api/stack-drift";
import { draftQuery, stackQuery } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { useEnvName } from "@/features/resources/use-env-name";
import { ReplaceDraftDialog } from "@/features/stack-drift/replace-draft-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

interface AcceptLiveActionProps {
  envId: string;
  stackId: string;
  // True when the newest check found drift to keep.
  drifted: boolean;
}

// The draft a person is asked about before it is replaced: who saved it, when, and the version the request will carry.
interface SavedDraft {
  version: number;
  savedBy: string;
  savedAt: string;
}

// AcceptLiveAction is "Accept live into draft" (D-23): it turns what runs into the stack's next draft and opens the
// Editor on it. It changes the draft and nothing else: no engine is written to and nothing is deployed, so the person
// reviews the draft and deploys it like any change. A saved draft with changes is shared work that the rebuild
// replaces, so it is only replaced after a confirm that names who saved it; a stack with none rebuilds at once. It
// needs Operator, waits while another stack operation holds the
// environment and has nothing to keep without drift; the reason is shown in each case and the server refuses the same
// things again.
export function AcceptLiveAction({ envId, stackId, drifted }: AcceptLiveActionProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const needs = useNeeds("stacks.update", envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const envName = useEnvName(envId);
  const accept = useAcceptLive(envId, stackId);
  const name = stack.data?.name ?? stackId;
  const [confirming, setConfirming] = useState<SavedDraft | null>(null);

  // rebuild asks the server to replace the draft, which it does only if the draft is still at version.
  async function rebuild(version: number) {
    if (accept.isPending) return;
    try {
      await accept.mutateAsync({ draftVersion: version });
      setConfirming(null);
      toastSuccess(t("stackAcceptLive.toast.rebuilt", { name }));
      void navigate({
        to: "/env/$envId/stacks/$stackId/editor",
        params: { envId, stackId },
        search: { pane: "files" },
      });
    } catch (e) {
      setConfirming(null);
      toastError(refusal(e instanceof AcceptLiveError ? e : new AcceptLiveError(0), t, envName));
    }
  }

  async function run() {
    if (accept.isPending) return;
    try {
      // The draft as it is now: what the dialog names and the version the request carries are the same read, so the
      // server refuses the rebuild if the draft moves after it.
      const draft = await queryClient.fetchQuery({ ...draftQuery(envId, stackId), staleTime: 0 });
      if (stack.data?.hasDraft && draft.isDraft) {
        setConfirming({ version: draft.version, savedBy: draft.savedBy, savedAt: draft.savedAt });
        return;
      }
      await rebuild(draft.version);
    } catch (e) {
      toastError(refusal(e instanceof AcceptLiveError ? e : new AcceptLiveError(0), t, envName));
    }
  }

  const reason = !needs.allowed
    ? (needs.reason ?? t("needs.role.operator"))
    : stack.data?.lock
      ? t("stackConverge.lock.reason", { env: envName })
      : drifted
        ? undefined
        : t("stackAcceptLive.noDrift");
  const label = accept.isPending ? t("stackAcceptLive.working") : t("stackAcceptLive.action");
  if (reason) {
    return (
      <NeedsTooltip reason={reason}>
        <Button type="button" variant="outline" className="max-md:h-11">
          <GitPullRequestArrow aria-hidden="true" />
          {label}
        </Button>
      </NeedsTooltip>
    );
  }
  return (
    <>
      <Button
        type="button"
        variant="outline"
        className="max-md:h-11"
        disabled={accept.isPending}
        onClick={() => void run()}
      >
        <GitPullRequestArrow aria-hidden="true" />
        {label}
      </Button>
      <ReplaceDraftDialog
        open={confirming !== null}
        onOpenChange={(open) => {
          if (!open) setConfirming(null);
        }}
        savedBy={confirming?.savedBy ?? ""}
        savedAt={confirming?.savedAt ?? ""}
        pending={accept.isPending}
        onReplace={() => confirming && void rebuild(confirming.version)}
      />
    </>
  );
}

// refusal is the sentence for an accept that did not happen.
function refusal(
  err: AcceptLiveError,
  t: (key: string, options?: Record<string, unknown>) => string,
  envName: string,
): string {
  const field = (location: string) =>
    err.fields.find((f) => f.location === location)?.message ?? "";
  switch (err.type) {
    case "rebuild_failed":
      return t("stackAcceptLive.errors.rebuildFailed", { reason: field("reason") });
    case "draft_conflict": {
      const user = field("savedBy");
      return user ? t("stackEditor.conflict.saved", { user }) : t("stackEditor.conflict.discarded");
    }
    case "operation_locked":
      return t("stackConverge.lock.reason", { env: envName });
    case "no_deployed_revision":
      return t("stackAcceptLive.errors.none");
    case "stack_stopped":
      return t("stackAcceptLive.errors.stopped");
    default:
      return err.status === 0
        ? t("stackAcceptLive.errors.lost")
        : t("stackAcceptLive.errors.generic");
  }
}
