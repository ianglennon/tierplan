// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { LoaderCircle } from "lucide-react";
import type { ReactNode } from "react";

import type { StackGit, StackGitReason } from "@/api/stack-git";
import { StatusDot } from "@/components/status-dot";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { shortSha } from "@/features/stack-git/sha-link";
import { useT } from "@/i18n/use-t";

type T = ReturnType<typeof useT>;

// What a sentence about why a sync failed may name. Every field is optional: the bell knows the stack and the code and
// little else, and a sentence falls back to a plainer wording when what it would name is missing.
export interface ReasonContext {
  // The host of the repository's address, for a host key.
  host?: string;
  // The branch, tag or commit the stack follows.
  ref?: string;
  // The stack's folder; empty or absent is the root of the repository.
  folder?: string;
  // The commit the sentence is about, in full or short form: the new head, for a check or an automatic deploy.
  sha?: string;
  // The server's own sentence on the reason, which carries no text from the git server. The first problem of a check
  // or the reason of an automatic deploy are in it.
  detail?: string;
}

// A detail as the end of a sentence: without its full stop, which the sentence brings.
const trimmed = (detail: string | undefined) => (detail ?? "").trim().replace(/\.$/, "");
// A server sentence that continues a colon: "The server refused the credential." reads "the server refused ...".
const lowered = (detail: string) => detail.charAt(0).toLowerCase() + detail.slice(1);

// Each reason code and the sentence the screens say for it (UI-SPEC "Sync error reasons"), one function per code. The
// map is typed over the generated union, so a code the server learns that has no sentence here fails tsc. The
// sentences are this table's own words: nothing of what a repository or its server said is shown except the first
// problem of a failed check and the reason of an automatic deploy, which are server sentences and are drawn as text.
// source_changed is not an error and has no sentence: it is a note on a stack that is behind.
const REASON: Record<StackGitReason, (t: T, c: ReasonContext) => string | null> = {
  fetch_failed: (t, c) => {
    const why = trimmed(c.detail);
    return why
      ? t("stackGit.reason.fetchFailedWhy", { reason: lowered(why) })
      : t("stackGit.reason.fetchFailed");
  },
  host_key_changed: (t, c) =>
    c.host
      ? t("stackGit.reason.hostKeyChanged", { host: c.host })
      : t("stackGit.reason.hostKeyChangedNoHost"),
  host_key_untrusted: (t, c) =>
    c.host
      ? t("stackGit.reason.hostKeyUntrusted", { host: c.host })
      : t("stackGit.reason.hostKeyUntrustedNoHost"),
  ref_gone: (t, c) =>
    c.ref ? t("stackGit.reason.refGone", { ref: c.ref }) : t("stackGit.reason.refGoneNoRef"),
  folder_gone: (t, c) =>
    c.folder && c.ref
      ? t("stackGit.reason.folderGone", { folder: c.folder, ref: c.ref })
      : t("stackGit.reason.folderGoneNoFolder"),
  checks_failed: (t, c) => {
    const problem = trimmed(c.detail);
    if (c.sha) {
      return problem
        ? t("stackGit.reason.checksFailed", { sha: shortSha(c.sha), problem })
        : t("stackGit.reason.checksFailedNoProblem", { sha: shortSha(c.sha) });
    }
    return problem
      ? t("stackGit.reason.checksFailedNoSha", { problem })
      : t("stackGit.reason.checksFailedNoShaNoProblem");
  },
  source_changed: () => null,
  auto_deploy_blocked: (t, c) => {
    const problem = trimmed(c.detail);
    if (c.sha) {
      return problem
        ? t("stackGit.reason.autoDeployBlocked", { sha: shortSha(c.sha), problem })
        : t("stackGit.reason.autoDeployBlockedNoProblem", { sha: shortSha(c.sha) });
    }
    return problem
      ? t("stackGit.reason.autoDeployBlockedNoSha", { problem })
      : t("stackGit.reason.autoDeployBlockedNoShaNoProblem");
  },
  auto_deploy_failed: (t, c) => {
    const why = trimmed(c.detail);
    if (c.sha) {
      return why
        ? t("stackGit.reason.autoDeployFailed", { sha: shortSha(c.sha), reason: why })
        : t("stackGit.reason.autoDeployFailedNoReason", { sha: shortSha(c.sha) });
    }
    return why
      ? t("stackGit.reason.autoDeployFailedNoSha", { reason: why })
      : t("stackGit.reason.autoDeployFailedNoShaNoReason");
  },
};

// reasonSentence is the sentence for a reason code, or null for a code that is no error (source_changed) or that this
// build does not know. A code from a newer server falls back to null and not to a raw key.
export function reasonSentence(
  t: T,
  code: string | undefined,
  context: ReasonContext,
): string | null {
  if (!code || !Object.hasOwn(REASON, code)) return null;
  return REASON[code as StackGitReason](t, context);
}

// reasonOf is the sentence for a git summary's own reason: the strip's reason line and the sync error alert say it.
// The commit it names is the head the last check saw.
export function reasonOfGit(t: T, git: StackGit): string | null {
  return reasonSentence(t, git.reasonCode, {
    host: git.repoHost,
    ref: git.ref,
    folder: git.folder,
    sha: git.headSha || git.deployedSha,
    detail: git.reasonDetail,
  });
}

// What the status word says. A check that runs hides the answer of the last one; an error hides the rest, since the
// last good version runs whatever the repository does.
export type SyncState =
  | "checking"
  | "error"
  | "source_changed"
  | "behind"
  | "fixed"
  | "in_sync"
  | "unknown";

export function syncStateOf(git: StackGit, checking: boolean): SyncState {
  if (checking) return "checking";
  if (git.status === "error") return "error";
  if (git.status === "behind")
    return git.reasonCode === "source_changed" ? "source_changed" : "behind";
  if (git.refKind === "commit") return "fixed";
  return git.status === "in_sync" ? "in_sync" : "unknown";
}

// The Tooltip on the status word, where there is something to add (UI-SPEC "Stack page (git stack)").
function tipOf(t: T, git: StackGit, state: SyncState): string | null {
  if (state === "in_sync" && git.headSha && git.deployedSha && git.headSha !== git.deployedSha) {
    return t("stackGit.status.inSyncTip", {
      ref: git.ref,
      sha: shortSha(git.headSha),
      folder: git.folder || t("stackGit.status.rootFolder"),
    });
  }
  if (state === "behind") {
    return t(git.behindCapped ? "stackGit.status.behindTipCapped" : "stackGit.status.behindTip", {
      count: git.behind,
      sha: shortSha(git.deployedSha),
      ref: git.ref,
      head: shortSha(git.headSha),
    });
  }
  return null;
}

// The words of the status, for a place that draws no dot: "Behind by 3 commits".
export function syncWord(t: T, git: StackGit, state: SyncState): string {
  switch (state) {
    case "checking":
      return t("stackGit.status.checking");
    case "error":
      return t("stackGit.status.error");
    case "source_changed":
      return t("stackGit.status.sourceChanged");
    case "behind":
      return t(git.behindCapped ? "stackGit.status.behindCapped" : "stackGit.status.behind", {
        count: git.behind,
      });
    case "fixed":
      return t("stackGit.status.fixed");
    case "in_sync":
      return t("stackGit.status.inSync");
    case "unknown":
      return t("stackGit.status.unknown");
  }
}

interface SyncStatusProps {
  git: StackGit;
  // True while a check runs: the summary says so, or Check now was just pressed.
  checking: boolean;
}

// SyncStatus is the status of a git stack against its repository (UI-SPEC "Color"): "In sync" and "Behind by n
// commits" and "Error" each with a dot, "Checking…" with a spinner, "Fixed commit" in muted text with no dot. The
// word is always there, so colour is never the only sign. An error has its reason under it in muted text; a stack that
// is behind because its source changed says so. A Tooltip on "In sync" and on "Behind" says what the head did.
export function SyncStatus({ git, checking }: SyncStatusProps) {
  const t = useT();
  const state = syncStateOf(git, checking);
  const word = syncWord(t, git, state);
  const tip = tipOf(t, git, state);
  const reason =
    state === "error"
      ? (reasonOfGit(t, git) ?? git.reasonDetail ?? null)
      : state === "source_changed"
        ? t("stackGit.status.sourceChangedNote")
        : null;

  let mark: ReactNode;
  switch (state) {
    case "checking":
      mark = (
        <span className="inline-flex items-center gap-1 text-sm font-semibold text-status-warning-text">
          <LoaderCircle
            className="size-4 animate-spin motion-reduce:animate-none"
            aria-hidden="true"
          />
          {word}
        </span>
      );
      break;
    case "fixed":
    case "unknown":
      mark = <span className="text-sm font-semibold text-muted-foreground">{word}</span>;
      break;
    default:
      mark = (
        <StatusDot
          state={state === "in_sync" ? "online" : state === "error" ? "offline" : "degraded"}
          label={word}
          className="text-sm font-semibold"
        />
      );
  }

  return (
    <div className="flex min-w-0 flex-col gap-1">
      {tip ? (
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              // biome-ignore lint/a11y/noNoninteractiveTabindex: the Tooltip must open on keyboard focus
              tabIndex={0}
              className="w-fit rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            >
              {mark}
            </span>
          </TooltipTrigger>
          <TooltipContent className="max-w-sm break-words">{tip}</TooltipContent>
        </Tooltip>
      ) : (
        mark
      )}
      {reason ? <p className="text-xs break-words text-muted-foreground">{reason}</p> : null}
    </div>
  );
}
