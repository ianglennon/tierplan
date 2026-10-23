// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Rocket } from "lucide-react";

import { stackQuery } from "@/api/stacks";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { StackDetailsCard } from "@/features/stacks/stack-details-card";
import { StackServicesCard } from "@/features/stacks/stack-services-card";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";

interface StackOverviewProps {
  envId: string;
  stackId: string;
}

// NotDeployed stands in the place of the Services table for a stack that has a definition and nothing
// running: it points at the editor, which every role that reads stacks may open, and at the review,
// which a user who may deploy goes on to.
function NotDeployed({ envId, stackId, name }: StackOverviewProps & { name: string }) {
  const t = useT();
  const deploy = useNeeds("stacks.deploy", envId);
  return (
    <Empty className="h-fit border">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Rocket aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={2}>
          {t("stackOverview.notDeployed.title")}
        </EmptyTitle>
        <EmptyDescription className="break-words">
          {t("stackOverview.notDeployed.body", { name })}
        </EmptyDescription>
      </EmptyHeader>
      <EmptyContent className="flex-row flex-wrap justify-center">
        <Button asChild variant="outline" className="max-md:h-11">
          <Link to="/env/$envId/stacks/$stackId/editor" params={{ envId, stackId }}>
            {t("stackOverview.notDeployed.openEditor")}
          </Link>
        </Button>
        {deploy.allowed ? (
          <Button asChild variant="outline" className="max-md:h-11">
            <Link to="/env/$envId/stacks/$stackId/review" params={{ envId, stackId }}>
              {t("stackOverview.notDeployed.deploy")}
            </Link>
          </Button>
        ) : null}
      </EmptyContent>
    </Empty>
  );
}

// StackOverview is the Overview tab (UI-SPEC "Stack page"): the Services card, or the Not deployed
// yet state in its place, beside the Details card, in two columns above 1024px and one below. The
// Details card never waits for Docker: it is the stack's own record. Every value renders as text.
export function StackOverview({ envId, stackId }: StackOverviewProps) {
  const { data } = useQuery(stackQuery(envId, stackId));
  if (!data) {
    return (
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2" aria-hidden="true">
        <Skeleton className="h-48 w-full" />
        <Skeleton className="h-48 w-full" />
      </div>
    );
  }
  return (
    <div className="grid grid-cols-1 items-start gap-6 lg:grid-cols-2">
      {data.state === "not_deployed" ? (
        <NotDeployed envId={envId} stackId={stackId} name={data.name} />
      ) : (
        <StackServicesCard
          envId={envId}
          stackId={stackId}
          name={data.name}
          target={data.target}
          busy={data.lastOperation?.state === "running"}
        />
      )}
      <StackDetailsCard stack={data} />
    </div>
  );
}
