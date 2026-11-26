// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronDown, Ellipsis, X } from "lucide-react";
import { type FormEvent, Fragment, useId, useState } from "react";

import {
  IngressActionError,
  type IngressDomain,
  ingressDomainsQuery,
  ingressStatusQuery,
} from "@/api/ingress";
import { useReleaseHostname, useReserveHostname } from "@/api/ingress-domains";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { RelativeTime } from "@/components/relative-time";
import { StatusDot } from "@/components/status-dot";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
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
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { AddDomainDialog } from "@/features/ingress/add-domain-dialog";
import { type Needs, useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// providerName is the DNS provider's name in sentence case: the words the app knows it by, else its id with a capital.
function providerName(id: string, t: (key: string, options?: Record<string, unknown>) => string) {
  const fallback = id === "" ? id : `${id.charAt(0).toUpperCase()}${id.slice(1)}`;
  return t(`ingressPage.domains.provider.${id}`, { defaultValue: fallback });
}

// DomainsTab is the Domains tab of the Ingress page (UIX-05, UI-SPEC S5): the domains stacks of the environment may
// serve hostnames under, each with where its certificates come from, which DNS provider proves the name, whether a
// working token is saved (never the token), how many certificates sit under it and which names are held back. It works
// while ingress is off (D-03): a domain can be registered before ingress is turned on, so the off state never replaces
// this tab.
export function DomainsTab({ envId }: { envId: string }) {
  const t = useT();
  const query = useQuery(ingressDomainsQuery(envId));
  const status = useQuery(ingressStatusQuery(envId));
  const needs = useNeeds("ingress.manage", envId);
  const [adding, setAdding] = useState(false);
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set());

  if (query.isPending) return <Loading />;
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("ingressPage.domains.error")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("ingressPage.domains.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const domains = query.data.items ?? [];
  const off = status.data?.enabled === false;

  function toggle(id: string) {
    const next = new Set(open);
    if (!next.delete(id)) next.add(id);
    setOpen(next);
  }

  return (
    <div className="flex flex-col gap-3">
      {off ? (
        <p className="text-sm text-muted-foreground">{t("ingressPage.domains.offLine")}</p>
      ) : null}
      {domains.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle role="heading" aria-level={3}>
              {t("ingressPage.domains.empty.title")}
            </EmptyTitle>
            <EmptyDescription>{t("ingressPage.domains.empty.body")}</EmptyDescription>
          </EmptyHeader>
          {needs.allowed ? (
            <EmptyContent>
              <Button type="button" className="max-md:h-11" onClick={() => setAdding(true)}>
                {t("ingressPage.domains.add.button")}
              </Button>
            </EmptyContent>
          ) : null}
        </Empty>
      ) : (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <NeedsTooltip reason={needs.reason}>
              <Button
                type="button"
                className="max-md:h-11 md:ms-auto"
                onClick={() => setAdding(true)}
              >
                {t("ingressPage.domains.add.button")}
              </Button>
            </NeedsTooltip>
          </div>
          <Table aria-label={t("ingressPage.domains.label")}>
            <TableHeader>
              <TableRow>
                <TableHead scope="col">{t("ingressPage.domains.columns.domain")}</TableHead>
                <TableHead scope="col">{t("ingressPage.domains.columns.issuer")}</TableHead>
                <TableHead scope="col">{t("ingressPage.domains.columns.provider")}</TableHead>
                <TableHead scope="col">{t("ingressPage.domains.columns.token")}</TableHead>
                <TableHead scope="col">{t("ingressPage.domains.columns.certificates")}</TableHead>
                <TableHead scope="col">{t("ingressPage.domains.columns.actions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {domains.map((d) => {
                const expanded = open.has(d.id);
                const panel = `ingress-reserved-${d.id}`;
                return (
                  <Fragment key={d.id}>
                    <TableRow>
                      <TableCell className="align-top whitespace-normal">
                        <div className="flex items-start gap-1">
                          <Button
                            type="button"
                            variant="ghost"
                            size="icon-sm"
                            className="-ms-1 shrink-0 max-md:size-11 md:h-8"
                            aria-expanded={expanded}
                            aria-controls={expanded ? panel : undefined}
                            aria-label={t("ingressPage.domains.show", { suffix: d.suffix })}
                            onClick={() => toggle(d.id)}
                          >
                            <ChevronDown
                              aria-hidden="true"
                              className={cn(
                                "motion-safe:transition-transform",
                                expanded ? "rotate-180" : "-rotate-90",
                              )}
                            />
                          </Button>
                          <span className="min-w-0 pt-1 font-mono text-xs break-all">
                            {d.suffix}
                          </span>
                        </div>
                      </TableCell>
                      <TableCell className="align-top text-sm whitespace-normal">
                        <IssuerCell issuer={d.issuer} />
                      </TableCell>
                      <TableCell className="align-top text-sm whitespace-normal">
                        {providerName(d.dnsProvider, t)}
                      </TableCell>
                      <TableCell className="align-top whitespace-normal">
                        <TokenCell domain={d} />
                      </TableCell>
                      <TableCell className="align-top text-sm whitespace-normal">
                        {d.certificates}
                      </TableCell>
                      <TableCell className="align-top">
                        <DomainActions domain={d} needs={needs} />
                      </TableCell>
                    </TableRow>
                    {expanded ? (
                      <TableRow className="hover:bg-transparent">
                        <TableCell
                          colSpan={6}
                          id={panel}
                          className="bg-muted/50 p-4 whitespace-normal"
                        >
                          <ReservedNames envId={envId} domain={d} needs={needs} />
                        </TableCell>
                      </TableRow>
                    ) : null}
                  </Fragment>
                );
              })}
            </TableBody>
          </Table>
        </>
      )}
      <AddDomainDialog envId={envId} open={adding} onOpenChange={setAdding} />
    </div>
  );
}

function Loading() {
  const t = useT();
  return (
    <div>
      <span role="status" className="sr-only">
        {t("ingressPage.domains.loading")}
      </span>
      <Skeleton className="h-32 w-full" aria-hidden="true" />
    </div>
  );
}

// IssuerCell says where the domain's certificates come from. Staging is a badge and the word, never colour alone, so a
// domain whose certificates browsers do not trust is plain on the page (U-06).
function IssuerCell({ issuer }: { issuer: IngressDomain["issuer"] }) {
  const t = useT();
  if (issuer !== "staging") return <span>{t("ingressPage.domains.issuer.production")}</span>;
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <Badge variant="outline" className="shrink-0">
        {t("ingressPage.domains.issuer.staging")}
      </Badge>
    </span>
  );
}

// TokenCell is whether a DNS token is saved and since when. The token itself is never in the answer and never drawn.
function TokenCell({ domain: d }: { domain: IngressDomain }) {
  const t = useT();
  if (!d.tokenSet) {
    return <StatusDot state="degraded" label={t("ingressPage.domains.token.none")} />;
  }
  return (
    <span className="inline-flex flex-wrap items-center gap-x-1.5">
      <StatusDot state="online" label={t("ingressPage.domains.token.saved")} />
      {d.tokenUpdatedAt ? (
        <span className="text-xs text-muted-foreground">
          <RelativeTime value={d.tokenUpdatedAt} />
        </span>
      ) : null}
    </span>
  );
}

// DomainActions is a row's menu (UI-SPEC S5): replace the token, change the issuer, delete the domain. The items are
// drawn for everyone and, for a reader, disabled with the reason; the server checks ingress.manage again. The dialogs
// behind them come with the rest of the domain writes, so for a person who may manage they stay unavailable here.
function DomainActions({ domain: d, needs }: { domain: IngressDomain; needs: Needs }) {
  const t = useT();
  const items = ["replaceToken", "changeIssuer", "delete"] as const;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={t("ingressPage.domains.menu.actionsFor", { suffix: d.suffix })}
        >
          <Ellipsis aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {items.map((item) => (
          <NeedsTooltip key={item} reason={needs.reason}>
            <DropdownMenuItem className="max-md:min-h-11" disabled={needs.allowed}>
              {t(`ingressPage.domains.menu.${item}`)}
            </DropdownMenuItem>
          </NeedsTooltip>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// ReservedNames is the hostnames held back under one domain (UI-SPEC S5): each in mono with a button that lifts the
// hold at once, since lifting a hold is undone by holding the name again, and, below, one field to hold another. A
// refusal (a name outside the domain, one a stack already claims) reads as the server sent it, under the field. A
// reader sees every control, disabled with the reason.
function ReservedNames({
  envId,
  domain: d,
  needs,
}: {
  envId: string;
  domain: IngressDomain;
  needs: Needs;
}) {
  const t = useT();
  const reserve = useReserveHostname(envId);
  const release = useReleaseHostname(envId);
  const [name, setName] = useState("");
  const fieldId = useId();
  const names = d.reserved ?? [];
  const hostname = name.trim();
  const failure = reserve.isError ? reserve.error : release.isError ? release.error : undefined;

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!needs.allowed || hostname === "" || reserve.isPending) return;
    release.reset();
    reserve.mutate({ domainId: d.id, hostname }, { onSuccess: () => setName("") });
  }

  return (
    <div className="flex flex-col gap-3">
      <h3 className="text-sm font-semibold">{t("ingressPage.domains.reserved.title")}</h3>
      {names.length === 0 ? (
        <p className="text-xs text-muted-foreground">{t("ingressPage.domains.reserved.none")}</p>
      ) : (
        <ul className="flex flex-col gap-1">
          {names.map((n) => (
            <li key={n} className="flex items-center gap-1">
              <span className="font-mono text-xs break-all">{n}</span>
              <NeedsTooltip reason={needs.reason}>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  className="shrink-0 max-md:size-11"
                  aria-label={t("ingressPage.domains.reserved.release", { name: n })}
                  disabled={release.isPending}
                  onClick={() => {
                    reserve.reset();
                    release.mutate({ domainId: d.id, hostname: n });
                  }}
                >
                  <X aria-hidden="true" />
                </Button>
              </NeedsTooltip>
            </li>
          ))}
        </ul>
      )}
      <form noValidate onSubmit={submit} className="flex flex-col gap-1.5">
        <div className="flex flex-wrap items-end gap-2">
          <Field className="max-w-sm min-w-0 flex-1">
            <FieldLabel htmlFor={fieldId}>{t("ingressPage.domains.reserved.field")}</FieldLabel>
            <Input
              id={fieldId}
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              disabled={!needs.allowed || reserve.isPending}
              aria-invalid={reserve.isError}
              aria-describedby={`${fieldId}-helper${failure ? ` ${fieldId}-error` : ""}`}
              className="font-mono text-xs max-md:h-11"
            />
          </Field>
          <NeedsTooltip reason={needs.reason}>
            <Button
              type="submit"
              variant="outline"
              className="max-md:h-11"
              disabled={needs.allowed && (hostname === "" || reserve.isPending)}
            >
              {reserve.isPending ? <Spinner aria-hidden="true" /> : null}
              {t("ingressPage.domains.reserved.submit")}
            </Button>
          </NeedsTooltip>
        </div>
        <FieldDescription id={`${fieldId}-helper`}>
          {t("ingressPage.domains.reserved.helper", { suffix: d.suffix })}
        </FieldDescription>
        {failure ? (
          <p id={`${fieldId}-error`} role="alert" className="text-xs break-words text-destructive">
            {(failure instanceof IngressActionError ? failure.detail : undefined) ??
              t("common.mutationFailed")}
          </p>
        ) : null}
      </form>
    </div>
  );
}
