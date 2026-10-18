// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Lock } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

interface SelfContainerBadgeProps {
  // server for the container Bayplan runs in, agent for an agent's container, and
  // agent-service for the swarm service whose tasks are the agents.
  kind: "server" | "agent" | "agent-service";
}

// SelfContainerBadge marks a container Bayplan itself depends on: a neutral
// outline badge with a lock, "Bayplan (this instance)" or "Bayplan agent".
// Acting on one asks for more care (D-11), and the lock says so before anyone
// opens the menu. It is never coloured.
export function SelfContainerBadge({ kind }: SelfContainerBadgeProps) {
  const t = useT();
  return (
    <Badge variant="outline" className="shrink-0">
      <Lock aria-hidden="true" data-icon="inline-start" />
      {kind === "agent-service"
        ? t("services.list.self.agentService")
        : t(`containers.list.self.${kind}`)}
    </Badge>
  );
}
