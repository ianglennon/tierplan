// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Link } from "@tanstack/react-router";
import { TriangleAlert } from "lucide-react";

import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { useEnvName } from "@/features/resources/use-env-name";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

interface AgentUpdateNoticeProps {
  envId: string;
}

// AgentUpdateNotice is what an agent that refuses a call by its own policy turns
// into (Pitfall 13): an agent older than the server does not know the call.
// Someone who may open the environment's settings gets a link to where the agent
// is updated; everyone else is told to ask an administrator. It is the only
// words about the agent the UI shows, never a code.
export function AgentUpdateNotice({ envId }: AgentUpdateNoticeProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const canConfigure = usePermission("environments.configure", envId);
  const canReadSettings = usePermission("settings.read");
  const canOpen = canConfigure || canReadSettings;
  return (
    <Alert variant="warning" role="alert">
      <TriangleAlert aria-hidden="true" />
      <AlertTitle>{t("actions.agentUpdate.title")}</AlertTitle>
      <AlertDescription className="flex flex-col gap-1 text-foreground">
        <p>
          {t("actions.agentUpdate.lead")}{" "}
          {canOpen
            ? t("actions.agentUpdate.update", { environment })
            : t("actions.agentUpdate.ask")}
        </p>
        {canOpen ? (
          <p>
            <Link
              to="/settings/environments/$envId"
              params={{ envId }}
              className="inline-flex rounded-sm text-accent-ink underline underline-offset-3 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11 max-md:items-center"
            >
              {t("actions.agentUpdate.link")}
            </Link>
          </p>
        ) : null}
      </AlertDescription>
    </Alert>
  );
}
