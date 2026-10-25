// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { RotateCcw } from "lucide-react";

import { type StackDriftService, useDrift } from "@/api/stack-drift";
import { stackQuery } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";

interface DriftPageProps {
  envId: string;
  stackId: string;
}

// The services that drifted, one row each: the change in words and the service's name.
function ServiceRows({ services }: { services: StackDriftService[] }) {
  const t = useT();
  return (
    <ul className="flex flex-col gap-2" aria-label={t("stackDrift.list.label")}>
      {services.map((s) => (
        <li
          key={s.name}
          className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg px-3 py-2 text-sm ring-1 ring-foreground/10"
        >
          <span className="font-medium">{t(`stackDrift.list.${s.change}`)}</span>
          <span data-slot="service-name" className="font-mono text-sm font-medium break-all">
            {s.short}
          </span>
        </li>
      ))}
    </ul>
  );
}

// DriftPage is the page of a stack's drift (UI-SPEC "Drift page"): what runs on the environment against the revision
// that is deployed. It checks the stack when it opens and again when asked, and shows what Bayplan knows until the
// check comes back. Nothing here changes the stack.
export function DriftPage({ envId, stackId }: DriftPageProps) {
  const t = useT();
  const stack = useQuery(stackQuery(envId, stackId));
  const envName = useEnvName(envId);
  const drift = useDrift(envId, stackId);
  const name = stack.data?.name ?? stackId;
  const data = drift.data;
  return (
    <div className="mx-auto flex w-full max-w-[1152px] flex-col gap-6">
      <div className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-col gap-1">
          <h3 className="text-xl font-semibold break-words">
            {t("stackDrift.page.title", { name })}
          </h3>
          {data ? (
            <p className="text-sm text-muted-foreground">
              {t("stackDrift.page.body", { env: envName, revision: data.revision })}{" "}
              <RelativeTime value={data.deployedAt} />.
            </p>
          ) : null}
        </div>
        <div className="flex items-center gap-3">
          {data ? (
            <p className="text-xs text-muted-foreground">
              {t("stackDrift.page.checked")} <RelativeTime value={data.checkedAt} />
            </p>
          ) : null}
          <Button
            type="button"
            variant="ghost"
            className="max-md:h-11"
            disabled={drift.checking}
            onClick={drift.recheck}
          >
            <RotateCcw aria-hidden="true" />
            {drift.checking ? t("stackDrift.page.checkingButton") : t("stackDrift.page.checkAgain")}
          </Button>
        </div>
      </div>
      <p className="text-xs text-muted-foreground">{t("stackDrift.page.intro")}</p>
      {data?.state === "drifted" ? <ServiceRows services={data.services ?? []} /> : null}
    </div>
  );
}
