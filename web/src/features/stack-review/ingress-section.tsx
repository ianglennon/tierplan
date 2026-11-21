// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { RotateCw } from "lucide-react";

import type { StackReviewIngress } from "@/api/stacks";
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

interface IngressSectionProps {
  ingress: StackReviewIngress | undefined;
  // The kind of host the stack deploys to. A standalone host joins no ingress network.
  target?: string;
}

// The shared ingress network the deploy attaches routed services to (the reserved name of D-05).
const INGRESS_NETWORK = "tierplan-ingress";

const ROUTE_GLYPH = { added: "+", kept: "=", released: "−" } as const;
const CERT_STATE_KEY = {
  reused: "issued",
  will_issue: "willIssue",
  waiting: "waiting",
} as const;

const MONO = "align-top font-mono text-xs break-all whitespace-normal";

// NetworkChange words "Joins tierplan-ingress" with the network name in mono: the sentence comes from the copy
// with the name in it, and the name is cut out of it again to be drawn as code.
function NetworkChange({ change }: { change: "joins" | "leaves" }) {
  const t = useT();
  const text = t(`stackIngress.review.network.${change}`, { network: INGRESS_NETWORK });
  const at = text.indexOf(INGRESS_NETWORK);
  if (at < 0) return <>{text}</>;
  return (
    <>
      {text.slice(0, at)}
      <span className="font-mono text-xs">{INGRESS_NETWORK}</span>
      {text.slice(at + INGRESS_NETWORK.length)}
    </>
  );
}

// IngressSection is the Ingress card of the review's Changes tab (D-21, D-22): the routes a deploy adds, keeps and
// releases, the certificate each hostname will have and, on a swarm, the services that join or leave the ingress
// network and so get their tasks replaced. It is read-only and has no loading state of its own (the page already
// says it is checking the stack). A review with no route, certificate or attachment draws nothing. Warnings and
// violations are not drawn here: they arrive in the review's problems. Every value is text.
export function IngressSection({ ingress, target }: IngressSectionProps) {
  const t = useT();
  const routes = ingress?.routes ?? [];
  const certificates = ingress?.certificates ?? [];
  const network = target === "standalone" ? [] : (ingress?.network ?? []);
  if (routes.length === 0 && certificates.length === 0 && network.length === 0) return null;

  const count = (change: "added" | "kept" | "released") =>
    routes.filter((r) => r.change === change).length;
  const certNote = certificates.some((c) => c.state !== "reused");
  const none = t("stackIngress.review.none");

  return (
    <SettingsCard title={t("stackIngress.review.title")}>
      {routes.length > 0 ? (
        <p className="text-sm text-muted-foreground">
          {t("stackIngress.review.summary", {
            added: count("added"),
            kept: count("kept"),
            released: count("released"),
          })}
        </p>
      ) : null}
      {routes.length > 0 ? (
        <div className="flex min-w-0 flex-col gap-2">
          <h4 className="text-sm font-semibold">{t("stackIngress.review.routes.heading")}</h4>
          <Table aria-label={t("stackIngress.review.routes.label")}>
            <TableHeader>
              <TableRow>
                <TableHead scope="col" className="w-28">
                  {t("stackIngress.review.routes.columns.change")}
                </TableHead>
                <TableHead scope="col">
                  {t("stackIngress.review.routes.columns.hostname")}
                </TableHead>
                <TableHead scope="col">{t("stackIngress.review.routes.columns.path")}</TableHead>
                <TableHead scope="col">{t("stackIngress.review.routes.columns.service")}</TableHead>
                <TableHead scope="col">{t("stackIngress.review.routes.columns.port")}</TableHead>
                <TableHead scope="col">{t("stackIngress.review.routes.columns.scheme")}</TableHead>
                <TableHead scope="col">{t("stackIngress.review.routes.columns.note")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {routes.map((r) => (
                <TableRow key={`${r.hostname}${r.path}:${r.change}`}>
                  <TableCell className="align-top whitespace-nowrap">
                    <span aria-hidden="true" className="me-1 font-mono">
                      {ROUTE_GLYPH[r.change]}
                    </span>
                    {t(`stackIngress.review.routes.change.${r.change}`)}
                  </TableCell>
                  <TableCell className={MONO}>{r.hostname}</TableCell>
                  <TableCell className={MONO}>{r.path === "" ? "/" : r.path}</TableCell>
                  <TableCell className={MONO}>{r.service}</TableCell>
                  <TableCell className={MONO}>{r.port}</TableCell>
                  <TableCell className={MONO}>{r.scheme}</TableCell>
                  <TableCell className="align-top text-xs whitespace-normal text-muted-foreground">
                    {r.note || none}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : null}
      {certificates.length > 0 ? (
        <div className="flex min-w-0 flex-col gap-2">
          <h4 className="text-sm font-semibold">{t("stackIngress.review.certificates.heading")}</h4>
          <Table aria-label={t("stackIngress.review.certificates.label")}>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">
                  {t("stackIngress.review.certificates.columns.hostname")}
                </TableHead>
                <TableHead scope="col">
                  {t("stackIngress.review.certificates.columns.certificate")}
                </TableHead>
                <TableHead scope="col">
                  {t("stackIngress.review.certificates.columns.detail")}
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {certificates.map((c) => (
                <TableRow key={c.hostname}>
                  <TableCell className={MONO}>{c.hostname}</TableCell>
                  <TableCell className="align-top whitespace-nowrap">
                    {t(`stackIngress.review.certificates.state.${CERT_STATE_KEY[c.state]}`)}
                  </TableCell>
                  <TableCell className="align-top text-xs whitespace-normal text-muted-foreground">
                    {c.reason || none}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {certNote ? (
            <p className="text-xs text-muted-foreground">
              {t("stackIngress.review.certificates.footnote")}
            </p>
          ) : null}
        </div>
      ) : null}
      {network.length > 0 ? (
        <div className="flex min-w-0 flex-col gap-2">
          <h4 className="text-sm font-semibold">{t("stackIngress.review.network.heading")}</h4>
          <Table aria-label={t("stackIngress.review.network.label")}>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">
                  {t("stackIngress.review.network.columns.service")}
                </TableHead>
                <TableHead scope="col">{t("stackIngress.review.network.columns.change")}</TableHead>
                <TableHead scope="col">{t("stackIngress.review.network.columns.effect")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {network.map((n) => (
                <TableRow key={`${n.service}:${n.change}`}>
                  <TableCell className={MONO}>{n.service}</TableCell>
                  <TableCell className="align-top whitespace-normal">
                    <NetworkChange change={n.change} />
                  </TableCell>
                  <TableCell className="align-top whitespace-normal">
                    {n.replacesTasks ? (
                      <span className="inline-flex items-center gap-1.5">
                        <RotateCw aria-hidden="true" className="size-3.5" />
                        {t("stackIngress.review.network.replaces")}
                      </span>
                    ) : (
                      t("stackIngress.review.network.noRestart")
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <p className="text-xs text-muted-foreground">
            {t("stackIngress.review.network.footnote")}
          </p>
        </div>
      ) : null}
    </SettingsCard>
  );
}
