// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronRight, CircleX, Info, TriangleAlert } from "lucide-react";
import { useState } from "react";

import type { RouteCheck, SubnetFinding } from "@/api/address-space";
import { CopyableCode } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { containsCidr } from "@/features/networks/cidr";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How many findings show before the rest sit behind a button (D-16).
export const SHOWN_FINDINGS = 5;

// More nodes than this in one state are one line with a count (D-17).
const GROUPED_FROM = 3;

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What a finding says, in the sentence the UI-SPEC gives its kind. Every name, route and
// interface in it is text.
function findingText(t: Translate, f: SubnetFinding): string {
  switch (f.kind) {
    case "same_env":
      return t(f.node ? "subnetPicker.findings.sameEnvOnNode" : "subnetPicker.findings.sameEnv", {
        network: f.network ?? "",
        subnet: f.subnet ?? "",
        node: f.node ?? "",
      });
    case "other_env":
      return t("subnetPicker.findings.otherEnv", {
        network: f.network ?? "",
        subnet: f.subnet ?? "",
        environment: f.environment ?? "",
      });
    case "host_route":
      return t("subnetPicker.findings.hostRoute", {
        node: f.node ?? "",
        route: f.route ?? "",
        iface: f.iface ?? "",
      });
    case "common_range":
      return t("subnetPicker.findings.commonRange", { range: f.range ?? "" });
    case "outside_pool":
      return t("subnetPicker.findings.outsidePool", { pool: f.pool ?? "" });
  }
}

interface FindingRowProps {
  finding: SubnetFinding;
  // Whether this is only the range the environment's own pool lies in.
  ownPool: boolean;
}

function FindingRow({ finding, ownPool }: FindingRowProps) {
  const t = useT();
  const blocking = finding.severity === "blocking";
  // The common-range warning of a pool that itself lies in a range many networks use (the swarm's
  // default 10.0.0.0/8) is true of every subnet in that pool, including the one Coaming suggests,
  // so it is shown at the weight of a note. The words and the hidden "Warning" are unchanged.
  const quiet = ownPool && finding.kind === "common_range";
  const label = t(blocking ? "subnetPicker.findings.blocking" : "subnetPicker.findings.warning");
  return (
    <li className="flex items-start gap-2 text-sm" data-severity={finding.severity}>
      {blocking ? (
        <CircleX aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-status-offline-text" />
      ) : quiet ? (
        <Info aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
      ) : (
        <TriangleAlert
          aria-hidden="true"
          className="mt-0.5 size-4 shrink-0 text-status-warning-text"
        />
      )}
      <span className={cn("min-w-0 break-words", quiet && "text-muted-foreground")}>
        <span className="sr-only">{label}: </span>
        {findingText(t, finding)}
      </span>
    </li>
  );
}

interface FindingListProps {
  findings: readonly SubnetFinding[];
  // The CIDRs of the environment's own address pools, so the finding that every subnet of a pool
  // shares can be told from one about this subnet alone.
  pools?: readonly string[];
  // The list's id, for the Create anyway checkbox to point at.
  id?: string;
}

// FindingList is what the typed subnet collides with, blocking first as the server orders them: five
// show and the rest sit behind "Show {n} more findings". Nothing in it is interpreted as markup.
export function FindingList({ findings, pools = [], id }: FindingListProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  if (findings.length === 0) return null;
  const shown = findings.slice(0, SHOWN_FINDINGS);
  const rest = findings.slice(SHOWN_FINDINGS);
  const ownPool = (f: SubnetFinding) =>
    f.kind === "common_range" && f.range !== undefined
      ? pools.some((p) => containsCidr(f.range as string, p))
      : false;
  return (
    <div id={id} className="flex flex-col gap-2">
      <ul className="flex flex-col gap-2">
        {shown.map((f, i) => (
          // The server orders findings and a finding has no identity of its own.
          // biome-ignore lint/suspicious/noArrayIndexKey: a stable, server-ordered list
          <FindingRow key={i} finding={f} ownPool={ownPool(f)} />
        ))}
      </ul>
      {rest.length > 0 ? (
        <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-2">
          {open ? null : (
            <CollapsibleTrigger asChild>
              <Button type="button" variant="ghost" size="sm" className="w-fit max-md:h-11">
                <ChevronRight aria-hidden="true" />
                {t("subnetPicker.findings.more", { count: rest.length })}
              </Button>
            </CollapsibleTrigger>
          )}
          <CollapsibleContent>
            <ul className="flex flex-col gap-2">
              {rest.map((f, i) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: a stable, server-ordered list
                <FindingRow key={i} finding={f} ownPool={ownPool(f)} />
              ))}
            </ul>
          </CollapsibleContent>
        </Collapsible>
      ) : null}
    </div>
  );
}

// A group of nodes whose route check was skipped for the same reason.
interface SkipGroup {
  state: RouteCheck["state"];
  local: boolean;
  nodes: string[];
  // The command that adds the route mount to a swarm's agent service, the same for the group.
  fix?: string;
}

function groupSkipped(checks: readonly RouteCheck[]): SkipGroup[] {
  const groups = new Map<string, SkipGroup>();
  for (const c of checks) {
    if (c.state === "ok") continue;
    const key = `${c.state}|${c.local}`;
    const group = groups.get(key) ?? { state: c.state, local: c.local, nodes: [] };
    group.nodes.push(c.node);
    if (!group.fix && c.fix) group.fix = c.fix;
    groups.set(key, group);
  }
  return [...groups.values()];
}

// The reason a check was skipped, in words, for the state of the node's agent and where the
// check ran (D-17 as amended).
function whyText(t: Translate, group: SkipGroup): string {
  if (group.local) {
    return group.state === "unmounted"
      ? t("subnetPicker.routes.local")
      : t("subnetPicker.routes.failed");
  }
  switch (group.state) {
    case "old_agent":
      return t("subnetPicker.routes.oldAgent");
    case "unmounted":
      return t("subnetPicker.routes.unmounted");
    case "offline":
      return t("subnetPicker.routes.offline");
    default:
      return t("subnetPicker.routes.failed");
  }
}

interface RouteCheckNotesProps {
  checks: readonly RouteCheck[];
  envType: "swarm" | "standalone";
}

// RouteCheckNotes says, node by node, why host routes were not checked: an agent too old to
// report them, one that cannot read the route file because it is not mounted (with the fix),
// Coaming's own container, an agent that is not connected, an answer that could not be read. More
// than three nodes in one state are one line with their number. It is a note, never a stop.
export function RouteCheckNotes({ checks, envType }: RouteCheckNotesProps) {
  const t = useT();
  const groups = groupSkipped(checks);
  if (groups.length === 0) return null;
  return (
    <ul className="flex flex-col gap-2">
      {groups.map((group) => {
        const grouped = group.nodes.length > GROUPED_FROM;
        const why = whyText(t, group);
        const wheres = grouped
          ? [t("subnetPicker.routes.nodes", { count: group.nodes.length })]
          : group.nodes;
        const showsFix = group.state === "unmounted" && !group.local;
        return (
          <li key={`${group.state}|${group.local}`} className="flex flex-col gap-2 text-sm">
            {wheres.map((where) => (
              <p key={where} className="flex items-start gap-2">
                <Info aria-hidden="true" className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
                <span className="min-w-0 break-words">
                  <span className="sr-only">{t("subnetPicker.findings.note")}: </span>
                  {t("subnetPicker.routes.skipped", { where, why })}
                </span>
              </p>
            ))}
            {grouped ? (
              <p className="pl-6 text-xs break-words text-muted-foreground">
                {group.nodes.join(", ")}
              </p>
            ) : null}
            {showsFix && envType === "swarm" && group.fix ? (
              <div className="pl-6">
                <CopyableCode code={group.fix} label={t("subnetPicker.routes.unmountedFixName")} />
              </div>
            ) : null}
            {showsFix && envType === "standalone" ? (
              <p className="pl-6 text-muted-foreground">
                {t("subnetPicker.routes.unmountedStandalone")}
              </p>
            ) : null}
          </li>
        );
      })}
    </ul>
  );
}
