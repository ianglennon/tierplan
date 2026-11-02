// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { hasPermission, type Me, requirementFor, useMe } from "@/hooks/use-me";
import i18n from "@/i18n";
import { useT } from "@/i18n/use-t";

export interface Needs {
  // True when the signed-in user holds the permission.
  allowed: boolean;
  // Why not, in the words of the disabled-with-reason pattern (D-19): "Needs
  // Operator", "Needs Admin", "Needs the <extra> permission" or "Only an
  // instance admin can do this". Absent when allowed, and when the server does
  // not register the permission.
  reason?: string;
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// needsOf is the pure behind useNeeds. What a permission asks of a caller is
// read from the requirements /api/me lists from the server's registry
// (requirementFor), never from a matrix kept in the SPA, so the words cannot
// disagree with what the server enforces (D-15b, D-19).
export function needsOf(
  me: Me | undefined,
  perm: string,
  envId: string | undefined,
  t: Translate,
): Needs {
  if (hasPermission(me, perm, envId)) return { allowed: true };
  const need = requirementFor(me, perm);
  if (!need) return { allowed: false };
  switch (need.kind) {
    case "instance":
      return { allowed: false, reason: t("needs.instance") };
    case "extra": {
      const key = `needs.extra.${perm}`;
      return {
        allowed: false,
        reason: i18n.exists(key, { ns: "needs" })
          ? t(key)
          : t("needs.extra.generic", { permission: perm }),
      };
    }
    default:
      return need.role
        ? { allowed: false, reason: t(`needs.role.${need.role}`) }
        : { allowed: false };
  }
}

// useNeeds says whether the signed-in user holds a permission, on an
// environment when envId is given, and if not, what it takes. It only decides
// what to show; the server enforces every permission.
export function useNeeds(perm: string, envId?: string): Needs {
  const { data } = useMe();
  const t = useT();
  return needsOf(data, perm, envId, t);
}
