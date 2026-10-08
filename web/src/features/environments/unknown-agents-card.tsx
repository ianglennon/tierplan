// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { type UnknownAgentItem, unknownAgentsQuery } from "@/api/environments";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { InstallDialog } from "@/features/environments/install-dialog";
import { useInstanceAdmin } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// UnknownAgentsCard is "Agents Bayplan doesn't know" (D-31): after a restore, the
// agents that were installed after the backup and so were refused unknown_agent.
// It is for instance admins, who alone may read the list, and it is there only
// while the list is not empty. Show install command opens the same dialog the
// environment's page does, which makes a new join token when it is confirmed,
// because the tokens that were pending in the backup are not in this instance.
export function UnknownAgentsCard() {
  const t = useT();
  const admin = useInstanceAdmin();
  const unknown = useQuery({ ...unknownAgentsQuery, enabled: admin });
  const [envId, setEnvId] = useState<string | null>(null);
  const trigger = useRef<HTMLElement | null>(null);

  const items: UnknownAgentItem[] = unknown.data ?? [];
  if (!admin || items.length === 0) return null;

  return (
    <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
      <CardHeader>
        <h3 className="text-base font-semibold">{t("envsettings.unknownAgents.title")}</h3>
        <CardDescription>{t("envsettings.unknownAgents.body")}</CardDescription>
      </CardHeader>
      <CardContent className="min-w-0">
        <Table aria-label={t("envsettings.unknownAgents.tableLabel")}>
          <TableHeader>
            <TableRow>
              <TableHead>{t("envsettings.unknownAgents.columns.node")}</TableHead>
              <TableHead>{t("envsettings.unknownAgents.columns.environment")}</TableHead>
              <TableHead>{t("envsettings.unknownAgents.columns.lastAttempt")}</TableHead>
              <TableHead>{t("envsettings.unknownAgents.columns.ip")}</TableHead>
              <TableHead className="w-0">
                <span className="sr-only">{t("envsettings.unknownAgents.columns.actions")}</span>
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {items.map((a) => (
              <TableRow key={`${a.envId}/${a.node}`}>
                <TableCell className="font-mono text-xs">{a.node}</TableCell>
                <TableCell>{a.envName}</TableCell>
                <TableCell>
                  <RelativeTime value={a.lastAttempt} />
                </TableCell>
                <TableCell className="font-mono text-xs">{a.ip}</TableCell>
                <TableCell className="text-right">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className={TOUCH_HEIGHT}
                    aria-label={t("envsettings.unknownAgents.showInstallAria", {
                      node: a.node,
                      environment: a.envName,
                    })}
                    onClick={(e) => {
                      trigger.current = e.currentTarget;
                      setEnvId(a.envId);
                    }}
                  >
                    {t("envsettings.unknownAgents.showInstall")}
                  </Button>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
      <InstallDialog
        open={envId !== null}
        mode="rotate"
        envId={envId ?? undefined}
        returnFocusRef={trigger}
        onOpenChange={(open) => {
          if (!open) setEnvId(null);
        }}
      />
    </Card>
  );
}
