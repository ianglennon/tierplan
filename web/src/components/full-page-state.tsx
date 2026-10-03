// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { LucideIcon } from "lucide-react";
import { ShieldAlert } from "lucide-react";
import type { ReactNode } from "react";

import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// How the icon looks: the neutral default, the warning amber of a state that
// is mending (D-41), or the offline red.
export type FullPageTone = "default" | "warning" | "offline";

const TONE: Record<FullPageTone, string> = {
  default: "",
  warning: "text-status-warning-text",
  offline: "text-status-offline-text",
};

interface FullPageStateProps {
  icon: LucideIcon;
  heading: string;
  body: ReactNode;
  // Under the body and above the actions, for what a state adds to its
  // sentence, such as the reason text of an offline agent.
  detail?: ReactNode;
  // One or several buttons or links; several sit in a wrapping row.
  action?: ReactNode;
  footnote?: ReactNode;
  tone?: FullPageTone;
  // Pulses the icon, which stops under prefers-reduced-motion.
  pulse?: boolean;
}

// FullPageState fills the content area when a whole page cannot be shown: an
// environment that is offline or unsupported, one that is missing, or a page
// the user may not see (D-58). The shell around it keeps working.
export function FullPageState({
  icon: Icon,
  heading,
  body,
  detail,
  action,
  footnote,
  tone = "default",
  pulse,
}: FullPageStateProps) {
  return (
    <Empty className="min-h-[50svh] border-0">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Icon
            className={cn(
              "size-5",
              TONE[tone],
              pulse && "animate-pulse motion-reduce:animate-none",
            )}
            aria-hidden="true"
          />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={1} className="text-base">
          {heading}
        </EmptyTitle>
        <EmptyDescription>{body}</EmptyDescription>
      </EmptyHeader>
      {detail ? (
        <div className="flex w-full max-w-md min-w-0 flex-col gap-2 text-start">{detail}</div>
      ) : null}
      {action || footnote ? (
        <EmptyContent>
          {action ? <div className="flex flex-wrap justify-center gap-2">{action}</div> : null}
          {footnote ? <p className="text-xs text-muted-foreground">{footnote}</p> : null}
        </EmptyContent>
      ) : null}
    </Empty>
  );
}

// NoPermission is what a 403 renders (UI-SPEC "No permission").
export function NoPermission() {
  const t = useT();
  return (
    <FullPageState
      icon={ShieldAlert}
      heading={t("common.noPermission.heading")}
      body={t("common.noPermission.body")}
    />
  );
}
