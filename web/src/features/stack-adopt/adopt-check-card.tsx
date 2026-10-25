// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight, CircleCheck, CircleX, Info, TriangleAlert } from "lucide-react";
import { useState } from "react";

import type { AdoptCheck, AdoptRequestError } from "@/api/stack-adopt";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Skeleton } from "@/components/ui/skeleton";
import { SettingsCard } from "@/features/environments/settings-card";
import { TruncatedText } from "@/features/resources/resource-table";
import { RoundTripGaps } from "@/features/stack-adopt/round-trip-gaps";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

interface AdoptCheckCardProps {
  name: string;
  // What the stack runs on: a swarm recreates services, a standalone host containers.
  target: "swarm" | "standalone";
  // The check, once it has settled; absent while it runs and when it failed.
  check?: AdoptCheck;
  // True while the check runs, the first time and on Check again.
  checking: boolean;
  // Why the check failed, when it did.
  error?: AdoptRequestError | null;
  onRetry: () => void;
  // A line to show in place of a result when there is nothing to check yet (a pasted file that is still empty).
  idle?: string;
  // A number that goes up when the person asks to see the differences of a pasted file.
  showToken?: number;
}

// OwnershipOnlyRow says how many services (containers on a host) differ from what runs in nothing but Bayplan's
// ownership labels, which the first deploy puts on by recreating them, and lists their names under "Show services".
function OwnershipOnlyRow({
  names,
  target,
}: {
  names: string[];
  target: AdoptCheckCardProps["target"];
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  if (names.length === 0) return null;
  return (
    <Collapsible open={open} onOpenChange={setOpen} data-slot="ownership-only">
      <div className="flex flex-wrap items-start gap-x-3 gap-y-1 text-sm">
        <Info aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
        <p className="min-w-0 flex-1 break-words">
          {t(
            target === "swarm"
              ? "stackAdoptCheck.ownershipOnly.services"
              : "stackAdoptCheck.ownershipOnly.containers",
            { count: names.length },
          )}
        </p>
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className="flex items-center gap-1 rounded-sm text-accent-ink hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11"
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
            />
            {t("stackAdoptCheck.ownershipOnly.show")}
          </button>
        </CollapsibleTrigger>
      </div>
      <CollapsibleContent>
        <ul className="mt-2 flex flex-col gap-1 ps-7">
          {names.map((n) => (
            <li key={n} className="min-w-0">
              <TruncatedText mono>{n}</TruncatedText>
            </li>
          ))}
        </ul>
      </CollapsibleContent>
    </Collapsible>
  );
}

// AdoptCheckCard is the card "What happens on the first deploy" of the Adopt page (UI-SPEC, D-16, D-20): the check of
// the rebuilt definition against what runs. While it runs it is a skeleton with "Checking {name}…" and busy. A clean
// round trip is one success line; gaps are a warning line with the per-service list. Either way the services that
// differ in nothing but ownership labels are counted apart. The check changes nothing and never blocks Adopt.
export function AdoptCheckCard({
  name,
  target,
  check,
  checking,
  error,
  onRetry,
  idle,
  showToken,
}: AdoptCheckCardProps) {
  const t = useT();
  const settled = check && !checking ? check : undefined;
  // A pasted file with an error is not compared with what runs: the paste field lists what to fix.
  const blocked = settled?.blocked === true;
  return (
    <SettingsCard title={t("stackAdoptCheck.card.title")}>
      <div aria-busy={checking || undefined} className="flex min-w-0 flex-col gap-4">
        {checking ? (
          <div className="flex flex-col gap-3" data-testid="adopt-check-loading">
            <p className="text-sm text-muted-foreground">
              {t("stackAdoptCheck.card.checking", { name })}
            </p>
            <Skeleton className="h-5 w-2/3" aria-hidden="true" />
            <Skeleton className="h-16 w-full" aria-hidden="true" />
          </div>
        ) : null}

        {/* The one announcement of a settled result: it is filled in when the check ends. */}
        <div role="status" aria-live="polite" className="flex flex-col gap-1 text-sm">
          {idle && !checking && !error ? <p className="text-muted-foreground">{idle}</p> : null}
          {blocked ? (
            <p className="text-muted-foreground">{t("stackAdoptPaste.card.blocked")}</p>
          ) : null}
          {settled?.clean && !blocked ? (
            <p className="flex items-start gap-2 font-medium">
              <CircleCheck
                aria-hidden="true"
                className="mt-0.5 size-4 shrink-0 text-status-online"
              />
              <span className="min-w-0 break-words">{t("stackAdoptCheck.clean")}</span>
            </p>
          ) : null}
          {settled && !settled.clean && !blocked ? (
            <>
              <p className="flex items-start gap-2 font-medium">
                <TriangleAlert
                  aria-hidden="true"
                  className="mt-0.5 size-4 shrink-0 text-status-warning-text"
                />
                <span className="min-w-0 break-words">
                  {t("stackAdoptCheck.gaps.title", { count: settled.gapCount })}
                </span>
              </p>
              <p className="ps-6 text-muted-foreground">{t("stackAdoptCheck.gaps.sub")}</p>
            </>
          ) : null}
        </div>

        {error && !checking ? (
          <Alert variant="destructive" role="alert">
            <CircleX aria-hidden="true" />
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span className="font-medium">{t("stackAdoptCheck.error.title", { name })}</span>
              <span className="break-words">
                {error.status === 429
                  ? t("stackAdoptCheck.error.busy")
                  : error.type === "rebuild_failed" && error.detail
                    ? error.detail
                    : t("stackAdoptCheck.error.body")}
              </span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={onRetry}
              >
                {t("stackAdoptCheck.error.retry")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        {settled?.gaps?.length ? (
          <RoundTripGaps services={settled.gaps} showToken={showToken} />
        ) : null}
        {settled && !blocked ? (
          <OwnershipOnlyRow names={settled.ownershipOnly ?? []} target={target} />
        ) : null}
      </div>
    </SettingsCard>
  );
}
