// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Globe } from "lucide-react";

import type { Credential } from "@/api/registries";
import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";

// CredentialBadge says where a credential applies: "Instance" with a globe for every
// environment, or the name of the one environment it is limited to. The name is typed
// by a person, so it is drawn as text only.
export function CredentialBadge({ scope }: { scope: Credential["scope"] }) {
  const t = useT();
  if (scope.kind === "instance") {
    return (
      <Badge variant="outline">
        <Globe aria-hidden="true" />
        {t("registries.badge.instance")}
      </Badge>
    );
  }
  return (
    <Badge variant="outline" className="max-w-40">
      <span className="truncate">{scope.envName ?? ""}</span>
    </Badge>
  );
}
