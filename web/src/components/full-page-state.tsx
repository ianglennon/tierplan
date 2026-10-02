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

interface FullPageStateProps {
  icon: LucideIcon;
  heading: string;
  body: ReactNode;
  action?: ReactNode;
  footnote?: ReactNode;
}

// FullPageState fills the content area when a whole page cannot be shown: an
// environment that is offline or unsupported, one that is missing, or a page
// the user may not see (D-58). The shell around it keeps working.
export function FullPageState({ icon: Icon, heading, body, action, footnote }: FullPageStateProps) {
  return (
    <Empty className="min-h-[50svh] border-0">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <Icon className="size-5" aria-hidden="true" />
        </EmptyMedia>
        <EmptyTitle role="heading" aria-level={1} className="text-base">
          {heading}
        </EmptyTitle>
        <EmptyDescription>{body}</EmptyDescription>
      </EmptyHeader>
      {action || footnote ? (
        <EmptyContent>
          {action}
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
