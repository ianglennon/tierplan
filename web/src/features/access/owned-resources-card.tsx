// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";

import { type OwnedItem, type OwnedState, userOwnedQuery } from "@/api/owned";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ownedName } from "@/features/access/owned-name";
import { TransferOwnershipDialog } from "@/features/access/transfer-ownership-dialog";
import { Truncated } from "@/features/access/truncated";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A row is 36px, and 44px under 768px for touch (D-64).
const ROW = "h-9 max-md:h-11";
const CELL = "px-3 py-0";

// OwnedState says whether an automation runs. "Active" is plain text; a paused
// automation says why in the warning colour, and the text always carries the
// meaning, never the colour alone.
function StateText({ state }: { state: OwnedState }) {
  const t = useT();
  return (
    <span className={cn("whitespace-nowrap", state !== "active" && "text-status-warning-text")}>
      {t(`owned.state.${state}`)}
    </span>
  );
}

interface OwnedResourcesCardProps {
  userId: string;
  username: string;
}

// OwnedResourcesCard is the automations a user owns (D-14, D-22): git syncs and
// webhooks that run with their permissions, each with its environment and
// whether it is running or paused, and a Transfer… that hands it to someone who
// could run it. The card is not on the page at all while the user owns nothing.
// The table scrolls inside 320px however many rows there are.
export function OwnedResourcesCard({ userId, username }: OwnedResourcesCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const query = useQuery(userOwnedQuery(userId));
  const [transferring, setTransferring] = useState<OwnedItem | null>(null);
  const items = query.data ?? [];

  const refresh = () =>
    void queryClient.invalidateQueries({ queryKey: userOwnedQuery(userId).queryKey });

  const dialog = (
    <TransferOwnershipDialog
      item={transferring}
      onClose={() => setTransferring(null)}
      onDone={() => {
        setTransferring(null);
        refresh();
      }}
      onStale={refresh}
    />
  );

  if (query.isError && !query.data) {
    return (
      <SettingsCard title={t("owned.card.title")} id="owned">
        <Alert variant="destructive">
          <AlertTitle>{t("owned.card.loadFailed")}</AlertTitle>
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>{t("owned.card.loadFailedBody")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("owned.card.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      </SettingsCard>
    );
  }
  if (items.length === 0) return dialog;

  return (
    <>
      <SettingsCard title={t("owned.card.title")} id="owned">
        <p className="text-sm text-muted-foreground">{t("owned.card.caption", { username })}</p>
        <div className="max-h-80 overflow-x-auto overflow-y-auto rounded-xl ring-1 ring-foreground/10">
          <table
            data-slot="table"
            aria-label={t("owned.card.tableLabel")}
            className="w-full caption-bottom text-sm"
          >
            <TableHeader>
              <TableRow className={cn(ROW, "hover:bg-transparent")}>
                {(["automation", "type", "environment", "state"] as const).map((c) => (
                  <TableHead
                    key={c}
                    className="sticky top-0 z-10 h-12 bg-muted px-3 text-xs font-semibold text-muted-foreground"
                  >
                    {t(`owned.card.columns.${c}`)}
                  </TableHead>
                ))}
                <TableHead className="sticky top-0 z-10 h-12 w-24 bg-muted px-3 text-right text-xs font-semibold text-muted-foreground">
                  {t("owned.card.columns.actions")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((item) => (
                <TableRow key={`${item.kind}/${item.resourceId}`} className={ROW}>
                  <TableCell className={cn(CELL, "font-medium")}>
                    <Truncated text={ownedName(t, item)} />
                  </TableCell>
                  <TableCell className={CELL}>
                    <Truncated text={item.label} />
                  </TableCell>
                  <TableCell className={CELL}>
                    <Truncated text={item.envName} />
                  </TableCell>
                  <TableCell className={CELL}>
                    <StateText state={item.state} />
                  </TableCell>
                  <TableCell className={cn(CELL, "text-right")}>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="max-md:h-11"
                      aria-label={t("owned.card.transferLabel", { automation: ownedName(t, item) })}
                      onClick={() => setTransferring(item)}
                    >
                      {t("owned.card.transfer")}
                    </Button>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </table>
        </div>
      </SettingsCard>
      {dialog}
    </>
  );
}
