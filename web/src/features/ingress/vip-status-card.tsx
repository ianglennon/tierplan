// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Trans } from "react-i18next";

import type { HealthState } from "@/api/environments";
import type { IngressVIP, IngressVIPManager } from "@/api/ingress-nodes";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// nodeNames turns swarm node IDs into the host names the candidates and managers carry, and keeps the ID for a node
// neither lists, so a name is never blank.
export function nodeNames(vip: IngressVIP): (id: string) => string {
  const names = new Map<string, string>();
  for (const m of vip.managers ?? []) if (m.hostname !== "") names.set(m.nodeId, m.hostname);
  for (const c of vip.candidates ?? []) if (c.hostname !== "") names.set(c.nodeId, c.hostname);
  return (id) => names.get(id) ?? id;
}

// VIPStatusCard is the virtual IP's status on the Nodes tab (UI-SPEC S4, D-06): who holds the address, then the
// address, its router ID, how many times the key was made (never the key) and the last pass that kept the service as
// wanted.
export function VIPStatusCard({ vip }: { vip: IngressVIP }) {
  const t = useT();
  const name = nodeNames(vip);
  const holders = (vip.holder.nodes ?? []).map(name);
  const holder: { state: HealthState; text: string } =
    vip.holder.state === "one"
      ? { state: "online", text: t("ingressPage.nodes.vip.holder.one", { node: holders[0] ?? "" }) }
      : vip.holder.state === "many"
        ? { state: "degraded", text: t("ingressPage.nodes.vip.holder.many") }
        : { state: "offline", text: t("ingressPage.nodes.vip.holder.none") };
  const failed = vip.lastEnsure.error !== undefined && vip.lastEnsure.error !== "";
  return (
    <SettingsCard title={t("ingressPage.nodes.vip.title")}>
      <div className="flex flex-col gap-1 text-sm">
        <StatusDot state={holder.state} label={holder.text} />
        {vip.holder.state === "many" ? (
          <p className="text-xs break-words text-muted-foreground">{holders.join(", ")}</p>
        ) : null}
        {vip.holder.state === "none" && vip.holderlessSince ? (
          <p className="text-xs text-muted-foreground">
            <Trans
              i18nKey="ingressPage.nodes.vip.holder.since"
              ns="ingressPage"
              components={{ time: <RelativeTime value={vip.holderlessSince} /> }}
            />
          </p>
        ) : null}
      </div>
      <dl className="grid gap-x-6 gap-y-3 text-sm md:grid-cols-[max-content_1fr]">
        <dt className="text-muted-foreground">{t("ingressPage.nodes.vip.address")}</dt>
        <dd className="font-mono break-all">{vip.address}</dd>
        <dt className="text-muted-foreground">{t("ingressPage.nodes.vip.routerId")}</dt>
        <dd>
          <span className="font-mono">{vip.routerId}</span>
          {vip.routerDerived ? (
            <span className="ml-2 text-xs text-muted-foreground">
              {t("ingressPage.nodes.vip.routerDerived")}
            </span>
          ) : null}
        </dd>
        <dt className="text-muted-foreground">{t("ingressPage.nodes.vip.keyGeneration")}</dt>
        <dd>{vip.keyGeneration}</dd>
        <dt className="text-muted-foreground">{t("ingressPage.nodes.vip.lastCheck")}</dt>
        <dd className={failed ? "break-words text-status-warning-text" : undefined}>
          {vip.lastEnsure.at ? (
            <Trans
              i18nKey={
                failed
                  ? "ingressPage.nodes.vip.lastCheckFailed"
                  : "ingressPage.nodes.vip.lastCheckDone"
              }
              ns="ingressPage"
              values={{ error: vip.lastEnsure.error }}
              components={{ time: <RelativeTime value={vip.lastEnsure.at} /> }}
            />
          ) : failed ? (
            t("ingressPage.nodes.vip.lastCheckFailedNoTime", { error: vip.lastEnsure.error })
          ) : (
            t("ingressPage.nodes.vip.lastCheckNever")
          )}
        </dd>
      </dl>
      {vip.foreignRouter.detected ? (
        <Alert variant="warning">
          <AlertTitle>{t("ingressPage.nodes.vip.foreignRouter.title")}</AlertTitle>
          <AlertDescription>
            {vip.foreignRouter.sender
              ? t("bell.vip.foreignRouter.bodySender", {
                  sender: vip.foreignRouter.sender,
                  routerId: vip.routerId,
                })
              : t("bell.vip.foreignRouter.body", { routerId: vip.routerId })}
          </AlertDescription>
        </Alert>
      ) : null}
      {(vip.clockSkewNodes ?? []).length > 0 ? (
        <Alert variant="warning">
          <AlertTitle>{t("ingressPage.nodes.vip.clock.title")}</AlertTitle>
          <AlertDescription>
            {t("ingressPage.nodes.vip.clock.body", {
              nodes: (vip.clockSkewNodes ?? []).map(name).join(", "),
              window: vip.authWindowSeconds,
            })}
          </AlertDescription>
        </Alert>
      ) : null}
      <Managers vip={vip} />
    </SettingsCard>
  );
}

// The privileged-services opt-in is shown as a dot and a word: allowed is online, refused is offline because
// keepalived cannot run on that manager, and one whose agent did not answer is unknown.
const POLICY: Record<IngressVIPManager["state"], { state: HealthState; key: string }> = {
  allows: { state: "online", key: "ingressPage.nodes.vip.managers.allows" },
  refuses: { state: "offline", key: "ingressPage.nodes.vip.managers.refuses" },
  unknown: { state: "unknown", key: "ingressPage.nodes.vip.managers.unknown" },
};

function Managers({ vip }: { vip: IngressVIP }) {
  const t = useT();
  if (!vip.managersReadable) {
    return (
      <p className="text-sm text-muted-foreground">
        {t("ingressPage.nodes.vip.managers.unreadable")}
      </p>
    );
  }
  const managers = vip.managers ?? [];
  if (managers.length === 0) return null;
  return (
    <Table aria-label={t("ingressPage.nodes.vip.managers.label")}>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">{t("ingressPage.nodes.vip.managers.manager")}</TableHead>
          <TableHead scope="col">{t("ingressPage.nodes.vip.managers.privileged")}</TableHead>
          <TableHead scope="col">{t("ingressPage.nodes.vip.managers.note")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {managers.map((m) => {
          const policy = POLICY[m.state];
          return (
            <TableRow key={m.nodeId}>
              <TableCell className="align-top text-sm break-all whitespace-normal">
                {m.hostname !== "" ? m.hostname : m.nodeId}
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <StatusDot state={policy.state} label={t(policy.key)} />
              </TableCell>
              <TableCell className="align-top text-xs whitespace-normal text-muted-foreground">
                {m.localSocket ? (
                  t("ingressPage.nodes.vip.managers.localSocket")
                ) : m.state === "refuses" ? (
                  <Trans
                    i18nKey="ingressPage.nodes.vip.managers.setting"
                    ns="ingressPage"
                    components={{
                      setting: (
                        <span className="font-mono break-all">
                          TIERPLAN_AGENT_ALLOW_PRIVILEGED_SERVICES=true
                        </span>
                      ),
                    }}
                  />
                ) : null}
              </TableCell>
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}
