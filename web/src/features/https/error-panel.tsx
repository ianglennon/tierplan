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
import { Rich } from "@/features/https/state-alerts";
import { useT } from "@/i18n/use-t";
import { formatOffsetWords } from "@/lib/format";
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
  // True while TIERPLAN_ACME is on and the card is not paused: TierPlan keeps trying by itself, and a
  // restart does not hurry it, because the backoff is stored (D-07).
  keepsTrying?: boolean;
  // How far TierPlan's clock is from the CA's, set only while the status carries it: positive when
  // TierPlan is ahead. It adds one line after the fix and changes nothing else (D-07, D-29).
  clock?: { offsetSeconds: number };
}

// ErrorPanel is the last-run error panel (D-11, ACME-07): the classified headline and what to do, how
// many attempts failed and when the next one is, and the CA's own problem document under Details. The
// document is the CA's text, capped by the server, and is drawn as a text node only, never as HTML.
export function ErrorPanel({
  error,
  failures,
  values,
  quiet,
  fresh,
  keepsTrying = false,
  clock,
}: ErrorPanelProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const { headline, fix } = errorCopy(t, error.code, values);
  const detail = error.detail ?? "";
  const next = quiet || values.time === undefined ? null : String(values.time);

  return (
    <Alert variant="destructive" id={LAST_RUN_ID} role={fresh ? "alert" : "note"}>
      <CircleX aria-hidden="true" />
      <AlertTitle className="min-w-0 break-words">{headline}</AlertTitle>
      <AlertDescription className="min-w-0 break-words">
        <p>{fix}</p>
        {clock ? (
          <p data-testid="clock-hint">
            {t(
              clock.offsetSeconds >= 0
                ? "https.lastRun.clockHint"
                : "https.lastRun.clockHintBehind",
              { offset: formatOffsetWords(clock.offsetSeconds) },
            )}
          </p>
        ) : null}
      </AlertDescription>
      <div className="col-start-2 mt-2 flex min-w-0 flex-col gap-3">
        <p className="text-xs text-muted-foreground">
          <Sentence
            text={t("https.lastRun.failed", { when: SLOT })}
            slot={<RelativeTime value={error.at} />}
          />{" "}
          {failures > 0 ? <>{t("https.lastRun.attempts", { count: failures })} </> : null}
          {next ? t("https.lastRun.next", { time: next }) : null}
        </p>
        {keepsTrying ? (
          <p className="text-xs text-muted-foreground">
            <Rich text={t("https.envState.keepsTrying")} />
          </p>
        ) : null}
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
