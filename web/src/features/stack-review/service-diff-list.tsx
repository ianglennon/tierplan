// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ArrowRight, ChevronRight, RotateCcw, RotateCw } from "lucide-react";
import { type ReactNode, useEffect, useMemo, useState } from "react";

import type { StackReviewService } from "@/api/stacks";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { imageOf } from "@/features/service-editor/spec-paths";
import { diffSpecs } from "@/features/spec-editor/diff";
import { SpecDiff } from "@/features/spec-editor/spec-diff";
import { OwnershipOnlyGroup } from "@/features/stack-review/ownership-only-group";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A service row is open by default when this many services or fewer changed (UI-SPEC "Review").
const OPEN_BY_DEFAULT = 3;

interface ServiceDiffListProps {
  services: StackReviewService[];
  // The headings of the value columns of an expanded row, and its caption as a function of the service's name, when
  // they are not the review's own (the Adopt page's are "Running now" and "After the first deploy").
  headings?: { current?: string; next?: string };
  caption?: (name: string) => string;
  // A number that goes up when every row that can open should open ("Show differences" on the Adopt page). 0 or absent
  // leaves each row as it is.
  openAll?: number;
  // "standalone" words the Ownership labels only group for containers and not for services.
  context?: "standalone";
}

// The glyph beside a change's word. The word carries the meaning; the glyph is for scanning.
const GLYPH = { added: "+", changed: "~", removed: "−" } as const;

// The reference without its digest: the digest is what the Images card shows.
const withoutDigest = (ref: string) => ref.split("@")[0] ?? ref;

// ImageChange is the image a service row ends with: the old reference and the new one when the
// reference moved, "New digest" when only the digest did, the reference alone for a service that
// is added or removed. Docker's text, drawn as text.
function ImageChange({ service }: { service: StackReviewService }) {
  const t = useT();
  const from = imageOf(service.current);
  const to = imageOf(service.next);
  if (service.change === "added") return to ? <ImageText text={withoutDigest(to)} /> : null;
  if (service.change === "removed") return from ? <ImageText text={withoutDigest(from)} /> : null;
  if (withoutDigest(from) !== withoutDigest(to)) {
    return (
      <span className="flex flex-wrap items-center gap-1">
        <ImageText text={withoutDigest(from)} />
        <ArrowRight aria-hidden="true" className="size-3.5 text-muted-foreground" />
        <span className="sr-only">{t("stackReview.serviceDiff.imageTo")}</span>
        <ImageText text={withoutDigest(to)} />
      </span>
    );
  }
  return from !== to ? (
    <span className="text-xs text-muted-foreground">{t("stackReview.serviceDiff.newDigest")}</span>
  ) : null;
}

function ImageText({ text }: { text: string }) {
  return <span className="font-mono text-xs break-all">{text}</span>;
}

// HiddenChanges lists the settings whose value changed to or from one that is hidden: the path
// alone, as "Changed (hidden)". The server decided that they changed before it hid them and never
// sent the values, so there is nothing else to show (STK-06, T-07-69).
function HiddenChanges({ name, paths }: { name: string; paths: string[] }) {
  const t = useT();
  return (
    <div className="rounded-lg ring-1 ring-foreground/10">
      <Table>
        <caption className="sr-only">
          {t("stackReview.serviceDiff.hiddenCaption", { name })}
        </caption>
        <TableHeader>
          <TableRow>
            <TableHead scope="col" className="w-40">
              {t("stackReview.serviceDiff.changeColumn")}
            </TableHead>
            <TableHead scope="col">{t("stackReview.serviceDiff.settingColumn")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {paths.map((path) => (
            <TableRow key={path}>
              <TableCell className="align-top whitespace-nowrap">
                <span aria-hidden="true" className="me-1 font-mono">
                  ~
                </span>
                {t("stackReview.serviceDiff.hiddenChange")}
              </TableCell>
              <TableCell className="align-top font-mono text-xs break-all whitespace-normal">
                {path}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

interface RowProps extends Pick<ServiceDiffListProps, "headings" | "caption" | "openAll"> {
  service: StackReviewService;
  defaultOpen: boolean;
}

// A change the file did not make: a standalone deploy recreates this service because the one it
// shares its network, processes or volumes with is recreated, or stops it and starts it again
// because it depends on that one with restart: true (plan 07-22).
const isCarried = (s: StackReviewService) => s.change === "recreated" || s.change === "restarted";

// CarriedRow is such a service: the glyph, the word, the name in mono and the reason, with nothing to
// expand and no count of settings, because nothing about the service itself changes. The service
// that carries it is drawn as text.
function CarriedRow({ service }: { service: StackReviewService }) {
  const t = useT();
  const recreated = service.change === "recreated";
  const Glyph = recreated ? RotateCcw : RotateCw;
  const because = service.because ?? "";
  return (
    <li className="rounded-lg ring-1 ring-foreground/10">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
        <span className="flex shrink-0 items-center gap-1 font-medium">
          <Glyph aria-hidden="true" className="size-3.5" />
          {t(recreated ? "stackReview.changes.recreated" : "stackReview.changes.restarted")}
        </span>
        <span data-slot="service-name" className="font-mono text-sm font-medium break-all">
          {service.short}
        </span>
        <span className="text-xs text-muted-foreground">
          {t(
            recreated
              ? "stackReview.changes.recreatedBecause"
              : "stackReview.changes.restartedBecause",
            { because },
          )}
        </span>
      </div>
    </li>
  );
}

// ServiceRow is one service: the change in words with its glyph, the name in mono, how many
// settings changed and the image change. A changed or added service expands to the field-level
// difference; a removed one is its summary row and nothing more.
function ServiceRow({ service, defaultOpen, headings, caption, openAll }: RowProps) {
  const t = useT();
  const change = service.change as keyof typeof GLYPH;
  const [open, setOpen] = useState(defaultOpen);
  useEffect(() => {
    if (openAll) setOpen(true);
  }, [openAll]);
  const removed = service.change === "removed";
  const hidden = service.hiddenChanges ?? [];
  const rows = useMemo(
    () => diffSpecs(service.current, service.next),
    [service.current, service.next],
  );

  const summary: ReactNode = (
    <>
      <span className="flex shrink-0 items-center gap-1 font-medium">
        <span aria-hidden="true" className="font-mono">
          {GLYPH[change]}
        </span>
        {t(`stackReview.serviceDiff.${change}`)}
      </span>
      <span data-slot="service-name" className="font-mono text-sm font-medium break-all">
        {service.short}
      </span>
      {service.change === "changed" ? (
        <span className="text-xs text-muted-foreground">
          {t("stackReview.serviceDiff.settings", { count: service.settings })}
        </span>
      ) : null}
      <span className="ms-auto min-w-0">
        <ImageChange service={service} />
      </span>
    </>
  );

  if (removed) {
    return (
      <li className="rounded-lg bg-status-offline/8 ring-1 ring-foreground/10">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-sm">
          {summary}
        </div>
      </li>
    );
  }

  return (
    <li className="rounded-lg ring-1 ring-foreground/10">
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger asChild>
          <button
            type="button"
            className={cn(
              "flex w-full flex-wrap items-center gap-x-3 gap-y-1 rounded-lg px-3 py-2 text-start text-sm",
              "focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11",
              service.change === "added" && "bg-status-online/8",
            )}
          >
            <ChevronRight
              aria-hidden="true"
              className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
            />
            {summary}
          </button>
        </CollapsibleTrigger>
        <CollapsibleContent className="flex flex-col gap-3 border-t p-3">
          {rows.length > 0 || hidden.length === 0 ? (
            <SpecDiff
              name={service.short}
              current={service.current}
              next={service.next}
              create={service.change === "added"}
              headings={headings}
              caption={caption?.(service.short)}
            />
          ) : null}
          {hidden.length > 0 ? <HiddenChanges name={service.short} paths={hidden} /> : null}
        </CollapsibleContent>
      </Collapsible>
    </li>
  );
}

// ServiceDiffList is the Changes tab's list of services (STK-06, D-11): one collapsible row for
// each service a deploy would add, change or remove, the removed ones last, and between them the
// services a standalone deploy recreates or restarts because another is recreated. A first deploy lists
// every service as Added. Masked values never appear on either side: the server sends them
// hidden, and a change to a hidden one is only a path.
export function ServiceDiffList({
  services,
  headings,
  caption,
  openAll,
  context,
}: ServiceDiffListProps) {
  // The services that differ in nothing but Bayplan's ownership labels are their own group above the list
  // (UI-SPEC "Review additions"); a service with a real change as well stays in the list.
  const labelsOnly = useMemo(() => services.filter((s) => s.ownershipOnly), [services]);
  // The server lists added and changed services by name, then the ones a recreate carries, then
  // the removed ones; the order is kept here too, so a list that came in another order still ends
  // with what goes away.
  const ordered = useMemo(() => {
    const rest = services.filter((s) => !s.ownershipOnly);
    return [
      ...rest.filter((s) => s.change !== "removed" && !isCarried(s)),
      ...rest.filter(isCarried),
      ...rest.filter((s) => s.change === "removed"),
    ];
  }, [services]);
  if (ordered.length === 0 && labelsOnly.length === 0) return null;
  // Only a row that can open counts for whether they open: a carried one has nothing to show.
  const open = ordered.filter((s) => s.change === "added" || s.change === "changed").length;
  const list =
    ordered.length === 0 ? null : (
      <ul className="flex flex-col gap-2">
        {ordered.map((s) =>
          isCarried(s) ? (
            <CarriedRow key={s.name} service={s} />
          ) : (
            <ServiceRow
              key={s.name}
              service={s}
              defaultOpen={open <= OPEN_BY_DEFAULT}
              headings={headings}
              caption={caption}
              openAll={openAll}
            />
          ),
        )}
      </ul>
    );
  if (labelsOnly.length === 0) return list;
  return (
    <div className="flex flex-col gap-4">
      <OwnershipOnlyGroup services={labelsOnly} context={context} />
      {list}
    </div>
  );
}
