// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { GitBranch, RefreshCw } from "lucide-react";
import type { ReactNode } from "react";

import { useCheckStackGit } from "@/api/stack-git";
import type { StackDetail } from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ShaLink } from "@/features/stack-git/sha-link";
import { SyncStatus } from "@/features/stack-git/sync-status";
import { usePullAndReview } from "@/features/stacks/stack-actions";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// The bar's own box (UI-SPEC "Layout dimensions added by this phase"): a bordered card, 16px inside, 56px tall at
// least; the skeleton has the same, so the page does not jump when the stack arrives.
const STRIP =
  "flex min-h-14 flex-col gap-2 rounded-lg border bg-card p-4 md:flex-row md:flex-wrap md:items-start md:gap-x-8";

// Item is one labelled value of the strip: a 12/600 muted label over a 14/400 value.
function Item({
  label,
  className,
  children,
}: {
  label: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <div className={`flex min-w-0 flex-col gap-1 ${className ?? ""}`}>
      <dt className="text-xs font-semibold text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm break-words">{children}</dd>
    </div>
  );
}

// Clamped is text of any length that shows two lines and the whole in a Tooltip (UI-SPEC "Truncation"): a 200-character
// branch name or a long repository name wraps inside the strip and never pushes it wider. It is a text node. With a
// repository ID it is the link to that repository's sheet on the Git page.
function Clamped({ text, mono, repoId }: { text: string; mono?: boolean; repoId?: string }) {
  const className = `line-clamp-2 rounded-sm break-all focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none ${mono ? "font-mono" : ""}`;
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        {repoId ? (
          <Link
            to="/git"
            search={{ repo: repoId }}
            className={`${className} text-accent-ink hover:underline`}
          >
            {text}
          </Link>
        ) : (
          <span
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the whole text is in a Tooltip, which must open on keyboard focus
            tabIndex={0}
            className={className}
          >
            {text}
          </span>
        )}
      </TooltipTrigger>
      <TooltipContent className="max-w-sm break-all">{text}</TooltipContent>
    </Tooltip>
  );
}

// GitStripSkeleton is the strip while the stack's git summary is not there yet: a bar of the same height.
function GitStripSkeleton() {
  const t = useT();
  return (
    <section aria-label={t("stackGit.strip.label")} aria-busy="true" className={STRIP}>
      <Skeleton className="h-8 w-40" />
      <Skeleton className="h-8 w-24" />
      <Skeleton className="h-8 w-32" />
      <Skeleton className="h-8 w-24" />
    </section>
  );
}

interface GitStripProps {
  stack: StackDetail;
}

// GitStrip is the bar under a git stack's header (UI-SPEC "Stack page (git stack)", D-16): what the stack follows, the
// commit that is deployed, where it stands against the repository and when that was last checked, with Check now and a
// link to the Git settings. It is a labelled section; its status is a polite status region, so the change from
// "Checking…" to the answer is announced once and focus never moves. Under 768px it stacks with the status first and
// the actions full width, 44px tall. Every value is drawn as text, and the only link out is the commit link on a
// known host.
export function GitStrip({ stack }: GitStripProps) {
  const t = useT();
  const git = stack.git;
  const check = useCheckStackGit(stack.envId, stack.id, {
    onError: () => toastError(t("stackGit.check.failed")),
  });
  const canSeeRepo = useNeeds("git.read", stack.envId);
  const deploy = useNeeds("stacks.deploy", stack.envId);
  const review = usePullAndReview(stack);
  if (!git) return stack.source === "git" ? <GitStripSkeleton /> : null;

  const checking = git.checking || check.running;
  const fixed = git.refKind === "commit";
  return (
    <section aria-label={t("stackGit.strip.label")} className={STRIP}>
      <dl className="contents">
        <Item label={t("stackGit.strip.source")} className="md:max-w-80 md:min-w-40 md:flex-1">
          <span className="flex min-w-0 flex-col gap-1">
            <span className="flex min-w-0 items-start gap-1">
              <GitBranch className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
              <span className="flex min-w-0 flex-wrap items-baseline gap-x-1.5">
                <span className="min-w-0 basis-full sm:basis-auto">
                  <Clamped text={git.ref} mono />
                </span>
                <span className="text-muted-foreground">{t(`stackGit.kind.${git.refKind}`)}</span>
              </span>
            </span>
            <span className="min-w-0">
              <Clamped
                text={git.repoName || t("stackGit.strip.repoUnknown")}
                repoId={canSeeRepo.allowed ? git.repoId : undefined}
              />
            </span>
            {git.folder ? (
              <span className="min-w-0 text-xs">
                <Clamped text={git.folder} mono />
              </span>
            ) : null}
          </span>
        </Item>
        <Item label={t("stackGit.strip.deployed")}>
          {git.deployedSha ? (
            <ShaLink sha={git.deployedSha} repoHost={git.repoHost} repoPath={git.repoPath} />
          ) : (
            <span className="text-muted-foreground">{t("stackGit.strip.notDeployed")}</span>
          )}
        </Item>
        <Item
          label={t("stackGit.strip.status")}
          className="max-md:order-first md:max-w-80 md:min-w-40"
        >
          {/* The one region a screen reader announces: "Checking…" and then the answer. */}
          <div role="status">
            <SyncStatus git={git} checking={checking} />
          </div>
        </Item>
        <Item label={t("stackGit.strip.checked")}>
          {git.checkedAt ? (
            <RelativeTime value={git.checkedAt} />
          ) : (
            <span className="text-muted-foreground">{t("stackGit.strip.never")}</span>
          )}
        </Item>
      </dl>
      <div className="flex flex-wrap items-center gap-2 max-md:w-full md:ms-auto md:self-center">
        {git.status === "behind" && (deploy.allowed || deploy.reason) ? (
          <NeedsTooltip reason={deploy.allowed ? undefined : deploy.reason}>
            <Button
              type="button"
              variant="outline"
              className="max-md:h-11 max-md:flex-1"
              aria-disabled={review.pending}
              onClick={deploy.allowed ? () => void review.pull() : undefined}
            >
              {review.pending ? t("stackGit.pull.pulling") : t("stackGit.pull.reviewUpdate")}
            </Button>
          </NeedsTooltip>
        ) : null}
        {fixed ? null : (
          <Button
            type="button"
            variant="ghost"
            className="max-md:h-11 max-md:flex-1"
            disabled={checking}
            onClick={check.check}
          >
            <RefreshCw
              aria-hidden="true"
              className={checking ? "animate-spin motion-reduce:animate-none" : undefined}
            />
            {checking ? t("stackGit.strip.checking") : t("stackGit.strip.checkNow")}
          </Button>
        )}
        <GitSettingsLink envId={stack.envId} stackId={stack.id} />
      </div>
    </section>
  );
}

// The link to the Git tab.
export function GitSettingsLink({ envId, stackId }: { envId: string; stackId: string }) {
  const t = useT();
  return (
    <Button asChild variant="link" className="px-1 text-accent-ink max-md:h-11 max-md:flex-1">
      <Link to="/env/$envId/stacks/$stackId/git" params={{ envId, stackId }}>
        {t("stackGit.strip.settings")}
      </Link>
    </Button>
  );
}
