// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import type { ReactNode } from "react";

import type { SessionInfo } from "@/api/sessions";
import { RelativeTime } from "@/components/relative-time";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useT } from "@/i18n/use-t";
import { describeDevice } from "@/lib/user-agent";
import { cn } from "@/lib/utils";

// How many skeleton rows keep the layout stable while loading (UI-SPEC).
const SKELETON_ROWS = 6;

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

interface DeviceLabelProps {
  session: SessionInfo;
  current: boolean;
}

// deviceName is "{Browser} on {OS}", or "Unknown device" for an agent it cannot
// read. It is also the accessible name of the row's actions.
export function useDeviceName(): (session: SessionInfo) => string {
  const t = useT();
  return (session) => {
    const device = describeDevice(session.userAgent);
    return device
      ? t("sessions.device", { browser: device.browser, os: device.os })
      : t("sessions.unknownDevice");
  };
}

// DeviceLabel is the Device cell: the readable name cut to the column, with the
// whole user agent in a tooltip, so a raw agent can never widen the table. The
// tooltip text is a text node, never markup.
function DeviceLabel({ session, current }: DeviceLabelProps) {
  const t = useT();
  const name = useDeviceName()(session);
  const label = (
    <span
      // The full agent is in the tooltip, which must open on keyboard focus (UI-SPEC D-66).
      tabIndex={session.userAgent ? 0 : undefined}
      className="block max-w-56 truncate rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      {name}
    </span>
  );
  return (
    <div className="flex min-w-0 items-center gap-2">
      {session.userAgent ? (
        <Tooltip>
          <TooltipTrigger asChild>{label}</TooltipTrigger>
          <TooltipContent className="max-w-sm break-all">{session.userAgent}</TooltipContent>
        </Tooltip>
      ) : (
        label
      )}
      {current ? <Badge variant="secondary">{t("sessions.current")}</Badge> : null}
    </div>
  );
}

interface SessionsTableProps {
  // Absent while the sessions load.
  sessions?: readonly SessionInfo[];
  loading?: boolean;
  // The row action, such as a Revoke button. The cell is empty for a row that
  // returns nothing, which is how the current session gets none (UI-SPEC). With
  // no renderAction there is no actions column at all.
  renderAction?: (session: SessionInfo) => ReactNode;
}

// SessionsTable is the rows of sessions for the Sessions dialog (a user's own)
// and for the user page card (an admin looking at someone else's). Device, IP
// address and last activity; an unknown device and a missing address read
// "Unknown device" and "—" (UI-SPEC "partial").
export function SessionsTable({ sessions, loading, renderAction }: SessionsTableProps) {
  const t = useT();
  const columns = [
    { id: "device", header: t("sessions.columns.device") },
    { id: "ip", header: t("sessions.columns.ip") },
    { id: "lastActive", header: t("sessions.columns.lastActive") },
    ...(renderAction ? [{ id: "actions", header: t("sessions.columns.actions") }] : []),
  ];
  return (
    <div className="max-h-80 overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
      <table
        data-slot="table"
        aria-busy={loading || undefined}
        aria-label={t("sessions.tableLabel")}
        className="w-full caption-bottom text-sm"
      >
        <TableHeader>
          <TableRow className={cn(ROW, "hover:bg-transparent")}>
            {columns.map((c) => (
              <TableHead
                key={c.id}
                className={cn(
                  "sticky top-0 z-10 h-12 bg-muted px-3 text-xs font-semibold text-muted-foreground",
                  c.id === "actions" && "w-24 text-right",
                )}
              >
                {c.header}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {loading || !sessions
            ? Array.from({ length: SKELETON_ROWS }, (_, i) => (
                // Placeholder rows have no identity of their own.
                // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
                <TableRow key={i} className={ROW}>
                  {columns.map((c) => (
                    <TableCell key={c.id} className={CELL}>
                      <Skeleton className="h-4 w-24" aria-hidden="true" />
                    </TableCell>
                  ))}
                </TableRow>
              ))
            : sessions.map((s) => (
                <TableRow key={s.id} className={ROW}>
                  <TableCell className={CELL}>
                    <DeviceLabel session={s} current={s.current} />
                  </TableCell>
                  <TableCell className={cn(CELL, "tabular-nums")}>
                    {s.ip ? (
                      s.ip
                    ) : (
                      <span className="text-muted-foreground">{t("sessions.missing")}</span>
                    )}
                  </TableCell>
                  <TableCell className={CELL}>
                    <RelativeTime value={s.lastSeenAt} />
                  </TableCell>
                  {renderAction ? (
                    <TableCell className={cn(CELL, "text-right")}>{renderAction(s)}</TableCell>
                  ) : null}
                </TableRow>
              ))}
        </TableBody>
      </table>
    </div>
  );
}
