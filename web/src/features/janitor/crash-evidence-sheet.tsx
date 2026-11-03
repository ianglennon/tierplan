// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Check, Copy } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";

import {
  type JanitorEvidence,
  JanitorRequestError,
  type JanitorRunItem,
  useJanitorEvidence,
} from "@/api/janitor";
import { CopyableCode, copyText } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { useT } from "@/i18n/use-t";
import { formatAbsolute } from "@/lib/format";
import { toastError } from "@/lib/toast";

// How long the button says "Copied" before it goes back to "Copy log".
const COPIED_MS = 2000;

interface CrashEvidenceSheetProps {
  envId: string;
  runId: string;
  // The item of the run whose evidence is shown.
  item: JanitorRunItem;
  onClose: () => void;
  // Where the focus goes when the sheet has closed. The run's table gives it the button that opened
  // the sheet, which may not be the element the browser would pick.
  onCloseAutoFocus?: (event: Event) => void;
}

// CrashEvidenceSheet shows why a container the janitor removed had stopped (D-06, UI-SPEC "Crash
// evidence sheet"): the facts the engine gave and the last lines of its log, saved just before the
// removal. Everything in it is text: the log lines are Docker's output, drawn verbatim in a pre and
// never parsed, and every value is placed as text.
export function CrashEvidenceSheet({
  envId,
  runId,
  item,
  onClose,
  onCloseAutoFocus,
}: CrashEvidenceSheetProps) {
  const t = useT();
  const query = useJanitorEvidence(envId, runId, item.seq);
  const gone = query.error instanceof JanitorRequestError && query.error.notFound;
  let body: ReactNode;
  if (query.data) {
    body = <Evidence name={item.name} evidence={query.data} />;
  } else if (gone) {
    body = <p className="text-sm">{t("janitorRun.evidence.notFound")}</p>;
  } else if (query.isError) {
    body = (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("janitorRun.evidence.error.title")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {t("janitorRun.evidence.error.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  } else {
    body = (
      <div
        role="status"
        aria-label={t("janitorRun.evidence.loading")}
        className="flex flex-col gap-3"
      >
        <Skeleton className="h-5 w-64" />
        <Skeleton className="h-5 w-48" />
        <Skeleton className="h-40 w-full" />
      </div>
    );
  }
  return (
    <Sheet open onOpenChange={(open) => !open && onClose()}>
      <SheetContent
        className="w-full gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-3xl"
        onCloseAutoFocus={onCloseAutoFocus}
      >
        <SheetHeader className="border-b pe-12">
          <SheetTitle className="break-all">
            {t("janitorRun.evidence.title", { name: item.name })}
          </SheetTitle>
          <SheetDescription>{t("janitorRun.evidence.description")}</SheetDescription>
        </SheetHeader>
        <div className="flex min-h-0 min-w-0 flex-1 flex-col gap-6 overflow-y-auto px-4 py-4 md:px-6">
          {body}
        </div>
      </SheetContent>
    </Sheet>
  );
}

// Fact is one row of the facts list: a label and its value, with a dash for a value that is missing.
function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-xs font-semibold text-muted-foreground">{label}</dt>
      <dd className="min-w-0 text-sm break-words">{children}</dd>
    </>
  );
}

const orDash = (value: string | null | undefined): string => (value ? value : "—");

function Evidence({ name, evidence }: { name: string; evidence: JanitorEvidence }) {
  const t = useT();
  const when = (iso: string | null) => (iso ? orDash(formatAbsolute(iso)) : "—");
  return (
    <>
      <dl className="grid grid-cols-1 gap-x-4 gap-y-2 sm:grid-cols-[160px_minmax(0,1fr)]">
        <Fact label={t("janitorRun.evidence.facts.exitCode")}>{evidence.exitCode}</Fact>
        <Fact label={t("janitorRun.evidence.facts.error")}>{orDash(evidence.error)}</Fact>
        <Fact label={t("janitorRun.evidence.facts.oom")}>
          {evidence.oomKilled ? t("janitorRun.evidence.yes") : t("janitorRun.evidence.no")}
        </Fact>
        <Fact label={t("janitorRun.evidence.facts.started")}>{when(evidence.startedAt)}</Fact>
        <Fact label={t("janitorRun.evidence.facts.finished")}>{when(evidence.finishedAt)}</Fact>
        <Fact label={t("janitorRun.evidence.facts.image")}>
          <span className="font-mono text-xs break-all">{orDash(evidence.image)}</span>
        </Fact>
        <Fact label={t("janitorRun.evidence.facts.digest")}>
          {evidence.imageDigest ? (
            <CopyableCode
              code={evidence.imageDigest}
              label={t("janitorRun.evidence.digestLabel")}
            />
          ) : (
            "—"
          )}
        </Fact>
        <Fact label={t("janitorRun.evidence.facts.node")}>
          <span className="font-mono text-xs break-all">{orDash(evidence.node)}</span>
        </Fact>
      </dl>
      <LogTail name={name} evidence={evidence} />
    </>
  );
}

// LogTail is the log section: the saved lines in a box that scrolls on its own, or one sentence for
// why there are none.
function LogTail({ name, evidence }: { name: string; evidence: JanitorEvidence }) {
  const t = useT();
  const lines = evidence.logLines ?? [];
  const saved = evidence.logState === "saved" && lines.length > 0;
  return (
    <section className="flex min-w-0 flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-semibold">{t("janitorRun.evidence.log.heading")}</h3>
        {saved ? <CopyLog lines={lines} /> : null}
      </div>
      {saved ? (
        <>
          <p className="text-xs text-muted-foreground">{t("janitorRun.evidence.log.caption")}</p>
          {/* A pre is the right element for verbatim lines; the region role names the box. */}
          {/* biome-ignore lint/a11y/useSemanticElements: a section would not keep the lines verbatim */}
          <pre
            role="region"
            // A scrolling box has to be reachable by keyboard, so it takes the focus (WCAG 2.1.1).
            // biome-ignore lint/a11y/noNoninteractiveTabindex: scrollable region
            tabIndex={0}
            aria-label={t("janitorRun.evidence.log.region", { name })}
            className="max-h-96 min-w-0 max-w-full overflow-auto rounded-lg border bg-muted p-3 font-mono text-xs leading-relaxed whitespace-pre text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
          >
            {lines.join("\n")}
          </pre>
        </>
      ) : (
        <p className="text-sm text-muted-foreground">
          {t(
            `janitorRun.evidence.log.${evidence.logState === "saved" ? "empty" : evidence.logState}`,
          )}
        </p>
      )}
    </section>
  );
}

// CopyLog copies the lines, one to a row, and says so: the button reads Copied for two seconds and a
// polite status says it too.
function CopyLog({ lines }: { lines: readonly string[] }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLSpanElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  async function copy() {
    clearTimeout(timer.current);
    if (await copyText(lines.join("\n"), host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
    } else {
      setCopied(false);
      toastError(t("env.copy.failed"));
    }
  }
  return (
    <span ref={host} className="inline-flex items-center">
      <Button type="button" variant="ghost" size="sm" className="max-md:h-11" onClick={copy}>
        {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
        {copied ? t("janitorRun.evidence.log.copied") : t("janitorRun.evidence.log.copy")}
      </Button>
      <span role="status" className="sr-only">
        {copied ? t("janitorRun.evidence.log.copied") : ""}
      </span>
    </span>
  );
}
