// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { ChevronRight, Copy } from "lucide-react";
import { Fragment, useEffect, useRef, useState } from "react";

import type { StackJobOutputLine } from "@/api/stacks";
import { copyText } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

interface DeployOutputProps {
  lines: readonly StackJobOutputLine[];
  // Whether the deploy failed: the panel opens by itself, and stays as the person leaves it after.
  failed: boolean;
}

const CLOCK = new Intl.DateTimeFormat(undefined, {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
  hourCycle: "h23",
});

// clockOf is the HH:MM:SS a line was written at, in the browser's own time zone; a time that cannot
// be read leaves the line without one.
function clockOf(at: string): string {
  const date = new Date(at);
  return Number.isNaN(date.getTime()) ? "" : CLOCK.format(date);
}

// lineText is a line as it reads on the screen and in a copy: "{HH:MM:SS} {text}".
function lineText(line: StackJobOutputLine): string {
  const clock = clockOf(line.at);
  return clock ? `${clock} ${line.text}` : line.text;
}

// DeployOutput is the deploy's output log (UI-SPEC "Deploy console"): a collapsible "Output" that is
// closed while the deploy runs and after it worked, and opens by itself when it fails. The log is a
// role=log surface that announces nothing (the console announces outcomes), each line is
// "{HH:MM:SS} {text}" drawn as text, a secret shows as <hidden> because the server hid it, Wrap turns
// long lines into several, and Copy output copies what is on the screen.
export function DeployOutput({ lines, failed }: DeployOutputProps) {
  const t = useT();
  const [open, setOpen] = useState(failed);
  const [wrap, setWrap] = useState(false);
  const surface = useRef<HTMLDivElement>(null);

  // A failed deploy opens the panel once, when it fails; a person who closes it keeps it closed.
  useEffect(() => {
    if (failed) setOpen(true);
  }, [failed]);

  // A log that is read at its end follows new lines; one scrolled up to read stays where it is.
  const stuck = useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: the effect follows the number of lines, not the array
  useEffect(() => {
    const el = surface.current;
    if (el && stuck.current) el.scrollTop = el.scrollHeight;
  }, [lines.length, open]);

  async function copy() {
    const ok = await copyText(lines.map(lineText).join("\n"), surface.current?.parentElement);
    if (ok) toastSuccess(t("stackDeploy.output.copied"));
    else toastError(t("stackDeploy.output.copyFailed"));
  }

  return (
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      data-slot="deploy-output"
      className="flex flex-col gap-2"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <CollapsibleTrigger className="flex min-h-6 items-center gap-1 rounded-sm text-sm font-medium focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11">
          <ChevronRight
            aria-hidden="true"
            className="size-4 shrink-0 transition-transform data-[open=true]:rotate-90 motion-reduce:transition-none"
            data-open={open}
          />
          {t("stackDeploy.output.title")}
        </CollapsibleTrigger>
        <span className="text-xs text-muted-foreground">
          {t("stackDeploy.output.lines", { count: lines.length })}
        </span>
        {open ? (
          <span className="ms-auto flex items-center gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-pressed={wrap}
              className="max-md:h-11"
              onClick={() => setWrap((w) => !w)}
            >
              {t("stackDeploy.output.wrap")}
            </Button>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => void copy()}
            >
              <Copy aria-hidden="true" />
              {t("stackDeploy.output.copy")}
            </Button>
          </span>
        ) : null}
      </div>
      <CollapsibleContent>
        <div
          ref={surface}
          role="log"
          aria-label={t("stackDeploy.output.title")}
          aria-live="off"
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling log must be reachable by keyboard
          tabIndex={0}
          onScroll={(e) => {
            const el = e.currentTarget;
            stuck.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
          }}
          className={cn(
            "max-h-80 min-h-16 max-w-full overflow-auto rounded-md border bg-card p-3 font-mono text-xs leading-5 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
            wrap ? "break-all whitespace-pre-wrap" : "whitespace-pre",
          )}
        >
          {lines.length === 0 ? (
            <span className="text-muted-foreground">{t("stackDeploy.output.empty")}</span>
          ) : (
            lines.map((line) => (
              <Fragment key={line.seq}>
                <span className="text-muted-foreground">{clockOf(line.at)}</span> {line.text}
                {"\n"}
              </Fragment>
            ))
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
