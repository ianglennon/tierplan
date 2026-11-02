// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { Boxes } from "lucide-react";

import { FullPageState } from "@/components/full-page-state";
import { Button } from "@/components/ui/button";
import { useT } from "@/i18n/use-t";

interface NotASwarmProps {
  envId: string;
  envName: string;
}

// NotASwarm is what every swarm page shows for a standalone host, which has no services, nodes,
// secrets or configs (UI-SPEC "Copywriting"). The way back is the containers page, the one
// every environment has.
export function NotASwarm({ envId, envName }: NotASwarmProps) {
  const t = useT();
  return (
    <FullPageState
      icon={Boxes}
      heading={t("services.notASwarm.heading")}
      body={t("services.notASwarm.body", { environment: envName })}
      action={
        <Button asChild variant="outline">
          <Link to="/env/$envId/containers" params={{ envId }}>
            {t("services.notASwarm.back")}
          </Link>
        </Button>
      }
    />
  );
}
