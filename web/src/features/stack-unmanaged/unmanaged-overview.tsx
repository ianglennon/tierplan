// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CircleArrowUp } from "lucide-react";
import type { ReactNode } from "react";

import { type UnmanagedStack, unmanagedStackQuery } from "@/api/unmanaged-stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { SettingsCard } from "@/features/environments/settings-card";
import { useEnvName } from "@/features/resources/use-env-name";
import { StackServicesTable } from "@/features/stacks/stack-services-card";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface UnmanagedOverviewProps {
  envId: string;
  name: string;
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-words">{children}</dd>
    </>
  );
}

// UnmanagedDetails is the Details card of an unmanaged stack: where it is, how it runs, that TierPlan did not make
// it and the label it was found by. The label and its value are Docker's text, in mono and wrapped anywhere, so a
// long name never widens the page.
function UnmanagedDetails({ envId, stack }: { envId: string; stack: UnmanagedStack }) {
  const t = useT();
  const envName = useEnvName(envId);
  return (
    <SettingsCard title={t("stackUnmanagedPage.details.title")}>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
        <Row label={t("stackUnmanagedPage.details.environment")}>
          <Link
            to="/env/$envId/stacks"
            params={{ envId }}
            className="rounded-sm text-accent-ink hover:underline"
          >
            {envName}
          </Link>
        </Row>
        <Row label={t("stackUnmanagedPage.details.target")}>
          {t(`stackOverview.details.targets.${stack.target}`)}
        </Row>
        <Row label={t("stackUnmanagedPage.details.source")}>
          {t("stackUnmanagedPage.details.sourceValue")}
        </Row>
        <Row label={t("stackUnmanagedPage.details.foundBy")}>
          <span className="font-mono text-xs break-all">
            {t("stackUnmanagedPage.details.foundByValue", {
              label: stack.foundBy.label,
              value: stack.foundBy.value,
            })}
          </span>
        </Row>
        <Row label={t("stackUnmanagedPage.details.services")}>
          {t("stackUnmanagedPage.details.servicesValue", { count: stack.servicesTotal })}
        </Row>
      </dl>
    </SettingsCard>
  );
}

// AdoptToUpdate stands where Update stack stands on a stack TierPlan manages (D-09): a stack found on the engine
// has no revision to roll an update back to, so its stack-level action reads "Adopt to update…" and opens the Adopt
// page. It follows Adopt stack's rules: an Env Admin gets the link, anyone else sees it off with the role it takes,
// and a stack that cannot be adopted with the server's reason; a user whose permissions are not known yet sees
// nothing. Its services keep their own Update.
function AdoptToUpdate({
  envId,
  stack,
}: {
  envId: string;
  stack: Pick<UnmanagedStack, "name" | "adoptable" | "reason">;
}) {
  const t = useT();
  const needs = useNeeds("stacks.create", envId);
  if (!needs.allowed && !needs.reason) return null;
  const label = (
    <>
      <CircleArrowUp aria-hidden="true" />
      {t("updates.stack.action.adopt")}
    </>
  );
  if (needs.allowed && stack.adoptable) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Button asChild variant="outline" className="max-md:h-11">
            <Link
              to="/env/$envId/unmanaged-stacks/$name/adopt"
              params={{ envId, name: stack.name }}
            >
              {label}
            </Link>
          </Button>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm">
          {t("updates.stack.action.adoptTooltip")}
        </TooltipContent>
      </Tooltip>
    );
  }
  return (
    <NeedsTooltip reason={needs.allowed ? stack.reason || undefined : needs.reason}>
      <Button type="button" variant="outline" className="max-md:h-11" aria-disabled="true">
        {label}
      </Button>
    </NeedsTooltip>
  );
}

// UnmanagedOverview is the Overview tab of an unmanaged stack (UI-SPEC "Unmanaged stack page"): the services
// found by label beside the Details card, in two columns above 1024px and one below. The per-object actions stay
// on the service and container pages the names link to (D-09). The layout owns the header, the notes and the
// error and gone states, so this draws the tab for a stack that loaded and a skeleton until it has.
export function UnmanagedOverview({ envId, name }: UnmanagedOverviewProps) {
  const t = useT();
  const { data } = useQuery(unmanagedStackQuery(envId, name));
  if (!data) {
    return (
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2" aria-hidden="true">
        <Skeleton className="h-48 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }
  const rows = data.services ?? [];
  const updatable = data.update?.state === "update_available";
  return (
    <div className="flex flex-col gap-6">
      {updatable ? (
        <div className="flex justify-end max-md:[&>*]:w-full">
          <AdoptToUpdate envId={envId} stack={data} />
        </div>
      ) : null}
      <div className="grid grid-cols-1 items-start gap-6 lg:grid-cols-2">
        <SettingsCard title={t("stackOverview.services.title")}>
          {rows.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {t("stackUnmanagedPage.services.empty")}
            </p>
          ) : (
            <StackServicesTable envId={envId} rows={rows} swarm={data.target === "swarm"} />
          )}
        </SettingsCard>
        <UnmanagedDetails envId={envId} stack={data} />
      </div>
    </div>
  );
}
