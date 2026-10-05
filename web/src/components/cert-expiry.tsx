// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useNow } from "@/components/relative-time";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { formatAbsolute, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";

const DAY = 86_400_000;

// A certificate with fewer whole days than this left is in the warning colour.
export const CERT_WARN_DAYS = 7;

interface CertExpiryProps {
  // When the certificate expires (RFC 3339).
  notAfter: string;
  // True when the agent should have renewed it by now and has not.
  renewalFailing?: boolean;
}

// CertExpiry is "Expires in {n} days" (UI-SPEC CertExpiry): the whole days left,
// counted down from the shared clock, in the warning colour under 7 days with
// "Not renewing" when renewal is failing, and "Expired {time}" in the offline
// colour once it has run out. The absolute local date, time and zone are in
// a tooltip that opens on hover and on keyboard focus.
export function CertExpiry({ notAfter, renewalFailing }: CertExpiryProps) {
  const t = useT();
  const now = useNow();
  const end = Date.parse(notAfter);
  if (Number.isNaN(end)) return <span>{t("envsettings.nodes.missing")}</span>;

  const remaining = end - now;
  const expired = remaining <= 0;
  const days = Math.floor(remaining / DAY);
  const soon = !expired && days < CERT_WARN_DAYS;
  const text = expired
    ? t("envsettings.cert.expired", { when: formatRelative(notAfter, new Date(now), t) })
    : t("envsettings.cert.expiresIn", { count: days });

  return (
    <span className="inline-flex flex-wrap items-baseline gap-x-2">
      <Tooltip>
        <TooltipTrigger asChild>
          <time
            dateTime={notAfter}
            // biome-ignore lint/a11y/noNoninteractiveTabindex: the tooltip must open on keyboard focus (UI-SPEC Time)
            tabIndex={0}
            className={cn(
              "rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
              expired && "text-status-offline-text",
              soon && "text-status-warning-text",
            )}
          >
            {text}
          </time>
        </TooltipTrigger>
        <TooltipContent>{formatAbsolute(notAfter)}</TooltipContent>
      </Tooltip>
      {soon && renewalFailing ? (
        <span className="text-xs text-status-warning-text">
          {t("envsettings.cert.notRenewing")}
        </span>
      ) : null}
    </span>
  );
}
