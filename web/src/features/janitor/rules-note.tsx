// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronRight } from "lucide-react";
import { useState } from "react";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// What the janitor keeps when nothing has changed its policy: the newest two exited task containers
// per slot, and a day before anything stale is removed (D-03). The policy card passes the real ones.
export const DEFAULT_KEEP = 2;
export const DEFAULT_GRACE_HOURS = 24;

interface RulesNoteProps {
  // Open until the janitor has run once, then closed (UI-SPEC interpretation 18): a new person sees
  // what it removes and never touches without a docs visit. A person's own choice overrides it.
  firstRunDone: boolean;
  // The keep-newest and grace values the words quote.
  keep?: number;
  graceHours?: number;
}

function Rules({ heading, children }: { heading: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <h3 className="text-sm font-semibold">{heading}</h3>
      <ul className="flex list-disc flex-col gap-0.5 ps-5 text-sm text-muted-foreground">
        {children}
      </ul>
    </div>
  );
}

// RulesNote is the collapsible that says what the janitor removes, what it only reports and what it
// never touches, and how to protect something (D-12, UI-SPEC "Rules body").
export function RulesNote({
  firstRunDone,
  keep = DEFAULT_KEEP,
  graceHours = DEFAULT_GRACE_HOURS,
}: RulesNoteProps) {
  const t = useT();
  const [chosen, setChosen] = useState<boolean | undefined>();
  const open = chosen ?? !firstRunDone;
  return (
    <Collapsible open={open} onOpenChange={setChosen} className="rounded-lg border">
      <CollapsibleTrigger className="group flex min-h-9 w-full items-center gap-2 rounded-lg px-3 py-2 text-start text-sm font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring max-md:min-h-11">
        <ChevronRight
          aria-hidden="true"
          className={cn(
            "size-4 shrink-0 transition-transform motion-reduce:transition-none",
            open && "rotate-90",
          )}
        />
        {t("janitorStatus.rules.trigger")}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="flex flex-col gap-4 px-3 pt-1 pb-3">
          <Rules heading={t("janitorStatus.rules.removes.heading")}>
            <li>{t("janitorStatus.rules.removes.tasks", { keep })}</li>
            <li>{t("janitorStatus.rules.removes.standalone")}</li>
            <li>
              {t("janitorStatus.rules.removes.images", {
                grace: t("janitor.age.hours", { count: graceHours }),
              })}
            </li>
            <li>{t("janitorStatus.rules.removes.networks")}</li>
          </Rules>
          <Rules heading={t("janitorStatus.rules.reports.heading")}>
            <li>{t("janitorStatus.rules.reports.networks")}</li>
            <li>{t("janitorStatus.rules.reports.containers")}</li>
          </Rules>
          <Rules heading={t("janitorStatus.rules.never.heading")}>
            <li>{t("janitorStatus.rules.never.volumes")}</li>
            <li>{t("janitorStatus.rules.never.running")}</li>
            <li>{t("janitorStatus.rules.never.cache")}</li>
            <li>{t("janitorStatus.rules.never.kept")}</li>
            <li>{t("janitorStatus.rules.never.deploy")}</li>
          </Rules>
          <p className="text-sm text-muted-foreground">
            {t("janitorStatus.rules.labelBefore")}
            <code className="font-mono text-xs text-foreground">tierplan.keep=true</code>
            {t("janitorStatus.rules.labelAfter")}
          </p>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}
