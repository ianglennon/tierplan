// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronDown, Ellipsis, Info } from "lucide-react";
import { useRef, useState } from "react";
import { Trans } from "react-i18next";

import { IngressActionError, ingressStatusQuery } from "@/api/ingress";
import {
  type IngressCert,
  type IngressCertBudget,
  ingressCertificateBudgetQuery,
  ingressCertificatesQuery,
  useRetryCertificate,
} from "@/api/ingress-certs";
import { CertExpiry } from "@/components/cert-expiry";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime, useNow } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  RequestCertificateDialog,
  SwitchToACMEDialog,
  UploadCertificateDialog,
} from "@/features/ingress/certificate-dialogs";
import { CertificateSheet, certDot } from "@/features/ingress/certificate-sheet";
import { IngressOffEmpty } from "@/features/ingress/ingress-off-empty";
import { type Needs, useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// At most this many rows show until the person asks for the rest (UI-SPEC S3, the Routes tab's pattern).
const SHOWN = 10;

// sourceKey is the words of where a certificate comes from and, for an ordered one, which directory it is from.
function sourceKey(c: IngressCert): string {
  if (c.source === "upload") return "ingressPage.certs.source.upload";
  return c.issuer === "staging"
    ? "ingressPage.certs.source.staging"
    : "ingressPage.certs.source.production";
}

// hasRun says whether the page's clock is at or past a time. A time that does not parse has not run.
function hasRun(at: string | undefined, now: number): boolean {
  if (at === undefined) return false;
  const when = Date.parse(at);
  return !Number.isNaN(when) && when <= now;
}

// CertificatesTab is the Certificates tab of the Ingress page (UIX-03, UI-SPEC S3): every certificate of the
// environment with where it comes from, where it stands, when it was issued, when it expires and is renewed, and why
// the last attempt failed, under the week's budget of new certificates. A staging certificate says so in words and in
// a badge, never in colour alone (U-06). `host`, from a route's "View certificate" link, opens the tab filtered to one
// hostname (U-19); `onShowAll` takes it out of the address too.
export function CertificatesTab({
  envId,
  host,
  onShowAll,
}: {
  envId: string;
  host?: string;
  onShowAll?: () => void;
}) {
  const t = useT();
  const now = useNow();
  const query = useQuery(ingressCertificatesQuery(envId));
  const budget = useQuery(ingressCertificateBudgetQuery(envId));
  const status = useQuery(ingressStatusQuery(envId));
  const needs = useNeeds("ingress.manage", envId);
  const manage = needs.allowed;
  const [requesting, setRequesting] = useState(false);
  const [filter, setFilter] = useState(host ?? "");
  const [showing, setShowing] = useState(host);
  const [all, setAll] = useState(false);

  if (query.isPending) return <Loading />;
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("ingressPage.certs.error")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("ingressPage.certs.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const everything = query.data.items ?? [];
  if (everything.length === 0) {
    // A certificate can exist while ingress is off, so the off state is only for a tab with nothing in it.
    if (status.isPending) return <Loading />;
    if (status.data?.enabled === false) return <IngressOffEmpty envId={envId} />;
    return (
      <Empty>
        <EmptyHeader>
          <EmptyTitle role="heading" aria-level={3}>
            {t("ingressPage.certs.empty.title")}
          </EmptyTitle>
          <EmptyDescription>{t("ingressPage.certs.empty.body")}</EmptyDescription>
        </EmptyHeader>
        {manage ? (
          <EmptyContent>
            <Button type="button" className="max-md:h-11" onClick={() => setRequesting(true)}>
              {t("ingressPage.certs.request.button")}
            </Button>
            <RequestCertificateDialog
              envId={envId}
              open={requesting}
              onOpenChange={setRequesting}
            />
          </EmptyContent>
        ) : null}
      </Empty>
    );
  }

  const needle = filter.trim().toLowerCase();
  const rows = everything.filter((c) => needle === "" || c.hostname.toLowerCase().includes(needle));
  const shown = all ? rows : rows.slice(0, SHOWN);
  const lines = budget.data?.items ?? [];

  return (
    <div className="flex flex-col gap-3">
      {lines.length > 0 ? (
        // biome-ignore lint/a11y/useSemanticElements: a labelled group of text lines, not a form field set
        <div
          role="group"
          aria-label={t("ingressPage.certs.budget.label")}
          className="flex flex-col gap-1 rounded-md bg-muted/50 p-3 text-sm"
        >
          {lines.map((b) => (
            <BudgetLine key={`${b.registeredDomain}:${b.issuer}`} budget={b} />
          ))}
        </div>
      ) : null}
      <div className="flex flex-wrap items-center gap-2">
        <Input
          type="text"
          aria-label={t("ingressPage.certs.filter")}
          placeholder={t("ingressPage.certs.filterPlaceholder")}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className="max-w-sm max-md:h-11"
        />
        {showing !== undefined ? (
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            onClick={() => {
              setFilter("");
              setShowing(undefined);
              onShowAll?.();
            }}
          >
            {t("ingressPage.certs.showing", { host: showing })}
          </Button>
        ) : null}
        <NeedsTooltip reason={needs.reason}>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11 md:ms-auto"
            onClick={() => setRequesting(true)}
          >
            {t("ingressPage.certs.request.button")}
          </Button>
        </NeedsTooltip>
      </div>
      <RequestCertificateDialog envId={envId} open={requesting} onOpenChange={setRequesting} />
      <Table aria-label={t("ingressPage.certs.label")}>
        <TableHeader>
          <TableRow>
            <TableHead scope="col">{t("ingressPage.certs.columns.hostname")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.state")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.issued")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.expires")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.renewal")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.error")}</TableHead>
            <TableHead scope="col">{t("ingressPage.certs.columns.actions")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.length === 0 ? (
            <TableRow className="hover:bg-transparent">
              <TableCell colSpan={7} className="whitespace-normal">
                <div className="flex flex-wrap items-center gap-3">
                  <p className="text-sm break-words">
                    {t("ingressPage.certs.noMatch", { filter: filter.trim() })}
                  </p>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="max-md:h-11"
                    onClick={() => setFilter("")}
                  >
                    {t("ingressPage.certs.clearFilter")}
                  </Button>
                </div>
              </TableCell>
            </TableRow>
          ) : null}
          {shown.map((c) => (
            <CertRow key={c.id} cert={c} now={now} manage={manage} needs={needs} envId={envId} />
          ))}
        </TableBody>
      </Table>
      {rows.length > SHOWN ? (
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="self-start max-md:h-11"
          aria-expanded={all}
          onClick={() => setAll(!all)}
        >
          {all
            ? t("ingressPage.certs.showFewer")
            : t("ingressPage.certs.showAll", { count: rows.length })}
        </Button>
      ) : null}
    </div>
  );
}

function Loading() {
  const t = useT();
  return (
    <div>
      <span role="status" className="sr-only">
        {t("ingressPage.certs.loading")}
      </span>
      <Skeleton className="h-48 w-full" aria-hidden="true" />
    </div>
  );
}

// BudgetLine is one registered domain and issuer of the weekly budget (D-12): how many new certificates the week has
// used of the cap, and, when none is left, when a slot opens, and, when other names count against the same budget,
// that they do. The no-headroom sentence is left out when no slot time is known.
function BudgetLine({ budget: b }: { budget: IngressCertBudget }) {
  const t = useT();
  const values = { domain: b.registeredDomain, used: b.used, cap: b.cap };
  return (
    <p>
      {t(
        b.issuer === "staging"
          ? "ingressPage.certs.budget.lineStaging"
          : "ingressPage.certs.budget.line",
        values,
      )}
      {b.headroom === 0 && b.nextSlotAt !== undefined ? (
        <>
          {" "}
          <Trans
            i18nKey="ingressPage.certs.budget.noHeadroom"
            ns="ingressPage"
            components={{ time: <RelativeTime value={b.nextSlotAt} /> }}
          />
        </>
      ) : null}
      {b.shared ? ` ${t("ingressPage.certs.budget.shared", { domain: b.registeredDomain })}` : ""}
    </p>
  );
}

function CertRow({
  cert: c,
  now,
  manage,
  needs,
  envId,
}: {
  cert: IngressCert;
  now: number;
  manage: boolean;
  needs: Needs;
  envId: string;
}) {
  const t = useT();
  // The page's clock is the judge of expiry: a certificate that ran out a second ago is Expired here even before the
  // server's next read says so, the same as the Expires cell beside it.
  const expired = (c.state === "valid" || c.state === "uploaded") && hasRun(c.expiresAt, now);
  const state = expired ? "expired" : c.state;
  const stateText = expired ? t("ingressPage.certs.expiredState") : c.stateText;
  return (
    <TableRow>
      <TableCell className="align-top whitespace-normal">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-xs break-all">{c.hostname}</span>
          {c.staging ? (
            <Badge variant="outline" className="shrink-0">
              {t("ingressPage.certs.staging")}
            </Badge>
          ) : null}
        </div>
        <p className="text-xs text-muted-foreground">{t(sourceKey(c))}</p>
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        <StatusDot state={certDot(state)} label={stateText} />
        {c.state === "dns_proven_on_staging" ? (
          <p className="text-xs break-words text-muted-foreground">
            {t("ingressPage.certs.stagingNeverServed")}
          </p>
        ) : c.queue ? (
          <p className="text-xs break-words text-muted-foreground">{c.queue.reason}</p>
        ) : !c.ready && c.expiresAt !== undefined ? (
          <p className="text-xs break-words text-muted-foreground">
            {t("ingressPage.certs.notServed")}
          </p>
        ) : null}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {c.issuedAt ? <RelativeTime value={c.issuedAt} /> : t("ingressPage.certs.none")}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {c.expiresAt ? (
          <CertExpiry notAfter={c.expiresAt} renewalFailing={c.state === "renewal_failed"} />
        ) : (
          t("ingressPage.certs.none")
        )}
      </TableCell>
      <TableCell className="align-top text-sm whitespace-normal">
        {c.renewAt ? (
          <span className="inline-flex flex-wrap items-center gap-x-1">
            {hasRun(c.renewAt, now) ? (
              <span>{t("ingressPage.certs.renewalDue")}</span>
            ) : (
              <RelativeTime value={c.renewAt} />
            )}
            {c.renewBasis ? <RenewBasis basis={c.renewBasis} /> : null}
          </span>
        ) : (
          t("ingressPage.certs.none")
        )}
      </TableCell>
      <TableCell className="align-top whitespace-normal">
        {c.lastError ? (
          <div className="flex flex-col gap-0.5">
            <p className="text-xs break-words">{c.lastError.hint}</p>
            <NextAttempt cert={c} />
            {manage && c.lastError.detail ? <ErrorDetail detail={c.lastError.detail} /> : null}
          </div>
        ) : (
          <span className="text-sm">{t("ingressPage.certs.none")}</span>
        )}
      </TableCell>
      <TableCell className="align-top">
        <CertActions cert={c} envId={envId} needs={needs} />
      </TableCell>
    </TableRow>
  );
}

// ErrorDetail is what the authority or the DNS provider answered, behind a "Show details" button. Only a person who
// may manage ingress is sent it (A-04), and only they are shown the control.
function ErrorDetail({ detail }: { detail: string }) {
  const t = useT();
  const [open, setOpen] = useState(false);
  return (
    <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col items-start gap-1">
      <CollapsibleTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="sm"
          className="-ms-2 h-7 px-2 text-xs max-md:h-11"
        >
          <ChevronDown
            aria-hidden="true"
            className={cn("motion-safe:transition-transform", open ? "rotate-180" : "-rotate-90")}
          />
          {t(open ? "ingressPage.certs.hideDetails" : "ingressPage.certs.showDetails")}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="w-full rounded-md bg-muted/50 p-2">
        <p className="font-mono text-xs break-all">{detail}</p>
      </CollapsibleContent>
    </Collapsible>
  );
}

// RenewBasis is the sentence the renewal time is based on, in a tooltip that opens on hover and on keyboard focus.
function RenewBasis({ basis }: { basis: string }) {
  const t = useT();
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          aria-label={t("ingressPage.certs.renewBasis")}
          className="inline-flex rounded-sm text-muted-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:size-11 max-md:items-center max-md:justify-center"
        >
          <Info aria-hidden="true" className="size-3.5" />
        </button>
      </TooltipTrigger>
      <TooltipContent>{basis}</TooltipContent>
    </Tooltip>
  );
}

// NextAttempt is when the order is tried again: the time the authority asked TierPlan to wait until when it did, else
// the next attempt TierPlan planned. A certificate with neither says nothing.
function NextAttempt({ cert: c }: { cert: IngressCert }) {
  const waiting = c.retryAfterUntil !== undefined;
  const at = c.retryAfterUntil ?? c.nextAttemptAt;
  if (at === undefined) return null;
  return (
    <p className="text-xs text-muted-foreground">
      <Trans
        i18nKey={waiting ? "ingressPage.certs.waitingUntil" : "ingressPage.certs.nextAttempt"}
        ns="ingressPage"
        components={{ time: <RelativeTime value={at} /> }}
      />
    </p>
  );
}

// CertActions is a row's menu (D-12, UI-SPEC S3). The write items are drawn for everyone and, for a reader, disabled
// with the reason; the server checks ingress.manage again. Retry is only for a certificate whose order failed, and
// switching back is only for one that was uploaded.
function CertActions({
  cert: c,
  envId,
  needs,
}: {
  cert: IngressCert;
  envId: string;
  needs: Needs;
}) {
  const t = useT();
  const retry = useRetryCertificate(envId);
  const [viewing, setViewing] = useState(false);
  const [uploading, setUploading] = useState(false);
  const [switching, setSwitching] = useState(false);
  // Set when an item was chosen, so closing the menu does not hand focus back to its button over the dialog that is
  // opening; the dialog returns focus to the button when it closes.
  const chose = useRef(false);
  const button = useRef<HTMLButtonElement>(null);
  const canRetry = c.state === "failed" || c.state === "renewal_failed";
  const canSwitch = c.source === "upload" && c.state !== "retired";
  const canUpload = c.state !== "retired";

  function choose(action: () => void) {
    return () => {
      chose.current = true;
      action();
    };
  }

  function retryNow() {
    retry.mutate(c.id, {
      onSuccess: () => toastSuccess(t("ingressPage.certs.retryDone", { host: c.hostname })),
      onError: (error) =>
        toastError(
          (error instanceof IngressActionError ? error.detail : undefined) ??
            t("common.mutationFailed"),
        ),
    });
  }

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            ref={button}
            type="button"
            variant="ghost"
            size="icon-sm"
            className="max-md:size-11"
            aria-label={t("ingressPage.certs.menu.actionsFor", { host: c.hostname })}
          >
            <Ellipsis aria-hidden="true" />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="end"
          onCloseAutoFocus={(event) => {
            if (!chose.current) return;
            chose.current = false;
            event.preventDefault();
          }}
        >
          <DropdownMenuItem className="max-md:min-h-11" onSelect={choose(() => setViewing(true))}>
            {t("ingressPage.certs.menu.view")}
          </DropdownMenuItem>
          {canRetry ? (
            <NeedsTooltip reason={needs.reason}>
              <DropdownMenuItem
                className="max-md:min-h-11"
                disabled={retry.isPending}
                onSelect={retryNow}
              >
                {t("ingressPage.certs.menu.retry")}
              </DropdownMenuItem>
            </NeedsTooltip>
          ) : null}
          {canUpload ? (
            <NeedsTooltip reason={needs.reason}>
              <DropdownMenuItem
                className="max-md:min-h-11"
                onSelect={choose(() => setUploading(true))}
              >
                {t("ingressPage.certs.menu.upload")}
              </DropdownMenuItem>
            </NeedsTooltip>
          ) : null}
          {canSwitch ? (
            <NeedsTooltip reason={needs.reason}>
              <DropdownMenuItem
                className="max-md:min-h-11"
                onSelect={choose(() => setSwitching(true))}
              >
                {t("ingressPage.certs.menu.switch")}
              </DropdownMenuItem>
            </NeedsTooltip>
          ) : null}
        </DropdownMenuContent>
      </DropdownMenu>
      <CertificateSheet
        cert={c}
        canManage={needs.allowed}
        open={viewing}
        onOpenChange={setViewing}
      />
      <UploadCertificateDialog
        envId={envId}
        host={c.hostname}
        open={uploading}
        onOpenChange={setUploading}
        returnFocusRef={button}
      />
      <SwitchToACMEDialog
        envId={envId}
        cert={c}
        open={switching}
        onOpenChange={setSwitching}
        returnFocusRef={button}
      />
    </>
  );
}
