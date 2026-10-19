// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ArrowRight, ChevronRight } from "lucide-react";
import { type ReactNode, useMemo, useState } from "react";

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
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// A service row is open by default when this many services or fewer changed (UI-SPEC "Review").
const OPEN_BY_DEFAULT = 3;

interface ServiceDiffListProps {
  services: StackReviewService[];
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

interface RowProps {
  service: StackReviewService;
  defaultOpen: boolean;
}

// ServiceRow is one service: the change in words with its glyph, the name in mono, how many
// settings changed and the image change. A changed or added service expands to the field-level
// difference; a removed one is its summary row and nothing more.
function ServiceRow({ service, defaultOpen }: RowProps) {
  const t = useT();
  const [open, setOpen] = useState(defaultOpen);
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
          {GLYPH[service.change]}
        </span>
        {t(`stackReview.serviceDiff.${service.change}`)}
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
            />
          ) : null}
          {hidden.length > 0 ? <HiddenChanges name={service.short} paths={hidden} /> : null}
        </CollapsibleContent>
      </Collapsible>
    </li>
  );
}

// ServiceDiffList is the Changes tab's list of services (STK-06, D-11): one collapsible row for
// each service a deploy would add, change or remove, the removed ones last. A first deploy lists
// every service as Added. Masked values never appear on either side: the server sends them
// hidden, and a change to a hidden one is only a path.
export function ServiceDiffList({ services }: ServiceDiffListProps) {
  // The server lists added and changed services by name and then the removed ones; the order is
  // kept here too, so a list that came in another order still ends with what goes away.
  const ordered = useMemo(
    () => [
      ...services.filter((s) => s.change !== "removed"),
      ...services.filter((s) => s.change === "removed"),
    ],
    [services],
  );
  if (ordered.length === 0) return null;
  return (
    <ul className="flex flex-col gap-2">
      {ordered.map((s) => (
        <ServiceRow key={s.name} service={s} defaultOpen={ordered.length <= OPEN_BY_DEFAULT} />
      ))}
    </ul>
  );
}
