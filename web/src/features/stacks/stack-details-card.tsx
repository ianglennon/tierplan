// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { Check, Copy, Loader2 } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";

import type { DriftView } from "@/api/stack-drift";
import type { StackGit } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { copyText } from "@/components/copyable-code";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { SettingsCard } from "@/features/environments/settings-card";
import { useEnvName } from "@/features/resources/use-env-name";
import { ShaLink } from "@/features/stack-git/sha-link";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

interface StackDetailsCardProps {
  stack: StackDetail;
  // What the page knows of the stack's drift, when it asked.
  drift?: DriftView;
}

// How long the copy button says it copied.
const COPIED_MS = 2000;

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  );
}

// CopyId is the stack ID in mono with a button that copies it. The result is told in words for a
// screen reader and by the icon for everyone else. With showId off the value is not drawn (the
// caller draws it, shortened, in extra) and the button still copies all of it.
function CopyId({
  id,
  label,
  copiedLabel,
  extra,
  showId = true,
}: {
  id: string;
  // The accessible name of the button and what is announced after it copied; the stack ID's by default.
  label?: string;
  copiedLabel?: string;
  // Something after the value and before the button, such as the link to a commit.
  extra?: ReactNode;
  // Draw id in full before extra; false when extra stands for it.
  showId?: boolean;
}) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLSpanElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(id, host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    toastError(t("env.copy.failed"));
  }

  return (
    <span ref={host} className="flex min-w-0 items-start gap-1">
      {showId ? <span className="min-w-0 font-mono text-xs break-all">{id}</span> : null}
      {extra}
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        className="-my-1 shrink-0 max-md:size-11"
        aria-label={label ?? t("stackOverview.details.copy")}
        onClick={() => void onCopy()}
      >
        {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
      </Button>
      <span role="status" className="sr-only">
        {copied ? (copiedLabel ?? t("stackOverview.details.copied")) : ""}
      </span>
    </span>
  );
}

// GitRows are the rows that replace Source for a stack from a repository (UI-SPEC "Stack page (git stack)"): where it
// lives, which commit is deployed and the newest the repository has, whether a commit is deployed on its own, and when
// the repository was last checked. Every value is the server's and is drawn as text; the only link out is the commit's
// on a known host.
function GitRows({ git }: { git: StackGit }) {
  const t = useT();
  const files = git.composeFiles ?? [];
  const behind = git.status === "behind" && git.headSha !== "";
  return (
    <>
      <Row label={t("stackGit.details.source")}>{t("stacks.source.git")}</Row>
      <Row label={t("stackGit.details.repository")}>
        {git.repoName || t("stackGit.strip.repoUnknown")}
      </Row>
      <Row label={t("stackGit.details.ref")}>
        <code className="font-mono text-xs break-all">{git.ref}</code>{" "}
        <span className="text-muted-foreground">{t(`stackGit.kind.${git.refKind}`)}</span>
      </Row>
      <Row label={t("stackGit.details.folder")}>
        {git.folder ? (
          <code className="font-mono text-xs break-all">{git.folder}</code>
        ) : (
          t("stackGit.details.rootFolder")
        )}
      </Row>
      <Row label={t("stackGit.details.composeFiles")}>
        {files.length > 0 ? (
          <ol className="flex flex-col">
            {files.map((f) => (
              <li key={f}>
                <code className="font-mono text-xs break-all">{f}</code>
              </li>
            ))}
          </ol>
        ) : (
          t("stackLists.missing")
        )}
      </Row>
      <Row label={t("stackGit.details.deployedCommit")}>
        {git.deployedSha ? (
          <CopyId
            id={git.deployedSha}
            label={t("stackGit.details.copyCommit")}
            copiedLabel={t("stackGit.details.copiedCommit")}
            showId={false}
            extra={
              <ShaLink sha={git.deployedSha} repoHost={git.repoHost} repoPath={git.repoPath} />
            }
          />
        ) : (
          t("stackGit.strip.notDeployed")
        )}
      </Row>
      {behind ? (
        <Row label={t("stackGit.details.latestCommit")}>
          <ShaLink sha={git.headSha} repoHost={git.repoHost} repoPath={git.repoPath} />
        </Row>
      ) : null}
      <Row label={t("stackGit.details.autoDeploy")}>
        {git.autoDeployPaused
          ? t("stackGit.details.autoDeployPaused")
          : git.autoDeploy
            ? t("stackGit.details.autoDeployOn")
            : t("stackGit.details.autoDeployOff")}
      </Row>
      <Row label={t("stackGit.details.checked")}>
        {git.checkedAt ? <RelativeTime value={git.checkedAt} /> : t("stackGit.strip.never")}
      </Row>
    </>
  );
}

// DriftValue is the Drift row (UI-SPEC "Drift"): the newest result the page has, else the stack's own summary of the
// newest check the server holds. While there is none it says it is checking, or that it could not.
function DriftValue({ stack, drift }: { stack: StackDetail; drift?: DriftView }) {
  const t = useT();
  const result = drift?.data ?? stack.drift;
  if (!result) {
    if (drift?.failed) return <>{t("stackDrift.details.failed")}</>;
    if (drift?.checking) {
      return (
        <span className="inline-flex items-center gap-1">
          <Loader2 aria-hidden="true" className="size-3.5 animate-spin" />
          {t("stackDrift.details.checking")}
        </span>
      );
    }
    return <>{t("stackDrift.details.unchecked")}</>;
  }
  switch (result.state) {
    case "none":
      return <>{t("stackDrift.details.none")}</>;
    case "drifted":
      return (
        <Link
          to="/env/$envId/stacks/$stackId/drift"
          params={{ envId: stack.envId, stackId: stack.id }}
          className="rounded-sm text-accent-ink hover:underline"
        >
          {t("stackDrift.details.differences", { count: result.count })}
        </Link>
      );
    case "stopped":
      return <>{t("stackDrift.details.stopped")}</>;
    case "not_deployed":
      return <>{t("stackDrift.details.notDeployed")}</>;
    default:
      return <>{t("stackDrift.details.failed")}</>;
  }
}

// StackDetailsCard is the Details card of the Overview (UI-SPEC "Stack page"): where the stack is,
// how it deploys, when it was made and last deployed, whether a draft waits, and its ID. A person
// and a name are the server's words, drawn as text.
export function StackDetailsCard({ stack, drift }: StackDetailsCardProps) {
  const t = useT();
  const envName = useEnvName(stack.envId);
  const by = (user: string) => t("stackOverview.details.by", { user });
  return (
    <SettingsCard title={t("stackOverview.details.title")}>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        <Row label={t("stackOverview.details.environment")}>
          <Link
            to="/env/$envId/stacks"
            params={{ envId: stack.envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {envName}
          </Link>
        </Row>
        <Row label={t("stackOverview.details.target")}>
          {t(`stackOverview.details.targets.${stack.target}`)}
        </Row>
        {stack.git ? (
          <GitRows git={stack.git} />
        ) : (
          <Row label={t("stackOverview.details.source")}>{t(`stacks.source.${stack.source}`)}</Row>
        )}
        <Row label={t("stackOverview.details.created")}>
          <RelativeTime value={stack.createdAt} /> {by(stack.createdBy)}
        </Row>
        <Row label={t("stackOverview.details.lastDeployed")}>
          {stack.lastDeployedAt ? (
            <>
              <RelativeTime value={stack.lastDeployedAt} /> {by(stack.lastDeployedBy)}
            </>
          ) : (
            t("stackOverview.details.never")
          )}
        </Row>
        <Row label={t("stackDrift.details.deployedRevision")}>
          {stack.deployedRevision ? (
            <>
              <Link
                to="/env/$envId/stacks/$stackId/history"
                params={{ envId: stack.envId, stackId: stack.id }}
                search={{ rev: stack.deployedRevision }}
                className="rounded-sm text-accent-ink hover:underline"
              >
                #{stack.deployedRevision}
              </Link>{" "}
              <RelativeTime value={stack.lastDeployedAt} /> {by(stack.lastDeployedBy)}
            </>
          ) : (
            t("stackDrift.details.never")
          )}
        </Row>
        <Row label={t("stackDrift.details.drift")}>
          <DriftValue stack={stack} drift={drift} />
        </Row>
        <Row label={t("stackOverview.details.draft")}>
          {stack.hasDraft ? (
            <>
              {t("stackOverview.details.draftSaved")}
              {stack.draftSavedAt ? (
                <>
                  {" "}
                  <RelativeTime value={stack.draftSavedAt} />
                </>
              ) : null}
              {stack.draftSavedBy ? <> {by(stack.draftSavedBy)}</> : null}
            </>
          ) : (
            t("stackOverview.details.noDraft")
          )}
        </Row>
        <Row label={t("stackOverview.details.id")}>
          <CopyId id={stack.id} />
        </Row>
      </dl>
    </SettingsCard>
  );
}
