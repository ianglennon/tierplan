// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { EnvironmentItem } from "@/api/environments";
import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

interface TypeBadgeProps {
  // What the environment is, from the agents or the local engine.
  envType?: EnvironmentItem["envType"];
  // The engine's own role. Only used to tell swarm from standalone when
  // envType is not there (the dashboard summary carries this and nothing else).
  type?: EnvironmentItem["type"];
  isLocalSocket?: boolean;
}

// TypeBadge names what an environment is (UI-SPEC TypeBadge, D-32): "Swarm" or
// "Standalone", plus a second outline badge "Local socket" for the host Coaming
// runs on. The engine roles (manager, worker) belong to the node list, not
// here. Neither badge ever truncates, and nothing shows until the type is known.
export function TypeBadge({ envType, type, isLocalSocket }: TypeBadgeProps) {
  const t = useT();
  const kind = envType ?? (type ? (type === "standalone" ? "standalone" : "swarm") : undefined);
  if (!kind && !isLocalSocket) return null;
  return (
    <>
      {kind ? (
        <Badge variant="outline" className="shrink-0">
          {t(`env.type.${kind}`)}
        </Badge>
      ) : null}
      {isLocalSocket ? (
        <Badge variant="outline" className="shrink-0">
          {t("env.type.localSocket")}
        </Badge>
      ) : null}
    </>
  );
}
