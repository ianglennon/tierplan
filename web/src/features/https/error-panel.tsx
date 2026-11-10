// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronRight, CircleX } from "lucide-react";
import { useState } from "react";

import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { type ACMELastError, errorCopy } from "@/features/https/copy";
import { Sentence, SLOT } from "@/features/https/sentence";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The anchor the status card's "Details below" link points at.
export const LAST_RUN_ID = "lets-encrypt-last-run";

interface ErrorPanelProps {
  error: ACMELastError;
  // Consecutive failed runs, as the ledger counts them.
  failures: number;
  // The values the copy names (host, port, profile, url and time), from errorValues.
  values: Record<string, string | number | undefined>;
  // True while renewals are paused or an attempt is refused: no next automatic attempt is promised.
  quiet: boolean;
  // True only on the render right after a run failed in this session; a failure read from the stored
  // ledger is a note, so a reload does not announce it again.
  fresh: boolean;
}

// ErrorPanel is the last-run error panel (D-11, ACME-07): the classified headline and what to do, how
// many attempts failed and when the next one is, and the CA's own problem document under Details. The
// document is the CA's text, capped by the server, and is drawn as a text node only, never as HTML.
export function ErrorPanel({ error, failures, values, quiet, fresh }: ErrorPanelProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const { headline, fix } = errorCopy(t, error.code, values);
  const detail = error.detail ?? "";
  const next = quiet || values.time === undefined ? null : String(values.time);

  return (
    <Alert variant="destructive" id={LAST_RUN_ID} role={fresh ? "alert" : "note"}>
      <CircleX aria-hidden="true" />
      <AlertTitle className="min-w-0 break-words">{headline}</AlertTitle>
      <AlertDescription className="min-w-0 break-words">{fix}</AlertDescription>
      <div className="col-start-2 mt-2 flex min-w-0 flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          <Sentence
            text={t("https.lastRun.failed", { when: SLOT })}
            slot={<RelativeTime value={error.at} />}
          />{" "}
          {failures > 0 ? <>{t("https.lastRun.attempts", { count: failures })} </> : null}
          {next ? t("https.lastRun.next", { time: next }) : null}
        </p>
        {detail === "" ? null : (
          <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-2">
            <CollapsibleTrigger asChild>
              <Button variant="ghost" size="sm" className="self-start max-md:h-11">
                <ChevronRight
                  aria-hidden="true"
                  className={cn(
                    "transition-transform motion-reduce:transition-none",
                    open && "rotate-90",
                  )}
                />
                {t("https.lastRun.details")}
              </Button>
            </CollapsibleTrigger>
            <CollapsibleContent>
              <pre
                data-testid="last-run-detail"
                className="max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-lg bg-muted p-4 font-mono text-xs text-foreground"
              >
                {detail}
              </pre>
            </CollapsibleContent>
          </Collapsible>
        )}
      </div>
    </Alert>
  );
}
