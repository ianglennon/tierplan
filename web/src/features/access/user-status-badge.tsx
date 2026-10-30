// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { UserStatus } from "@/api/users";
import { Badge } from "@/components/ui/badge";
import { useT } from "@/i18n/use-t";
import { formatClock } from "@/lib/format";

interface UserStatusBadgeProps {
  status: UserStatus;
  // While the account is locked: when the lock ends (RFC 3339).
  lockedUntil?: string;
}

// UserStatusBadge says what state a user is in. The server decides which state
// wins when several apply (locked, disabled, invite expired, invite pending,
// active); this only draws it. "Active" is plain text, every other state is a
// badge, and the text always carries the meaning, never the colour alone. A
// badge never truncates.
export function UserStatusBadge({ status, lockedUntil }: UserStatusBadgeProps) {
  const t = useT();
  switch (status) {
    case "locked":
      return (
        <Badge variant="outline" className="border-status-warning text-status-warning-text">
          {t("users.status.locked", { time: formatClock(lockedUntil) ?? "" }).trim()}
        </Badge>
      );
    case "invite_expired":
      return (
        <Badge variant="outline" className="border-status-warning text-status-warning-text">
          {t("users.status.invite_expired")}
        </Badge>
      );
    case "disabled":
      return <Badge variant="secondary">{t("users.status.disabled")}</Badge>;
    case "invite_pending":
      return <Badge variant="outline">{t("users.status.invite_pending")}</Badge>;
    default:
      return <span>{t("users.status.active")}</span>;
  }
}
