// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type ReactNode, useId } from "react";

import { Card, CardContent, CardHeader } from "@/components/ui/card";

interface SettingsCardProps {
  title: string;
  children: ReactNode;
  // An anchor for the section, such as "remove" for /settings/environments/{id}#remove.
  id?: string;
}

// SettingsCard is one section of an environment's settings page: a card whose
// title is a level 3 heading, inside a labelled region so assistive technology
// can jump between sections. 16px padding under 768px and 24px from there up
// (UI-SPEC Spacing).
export function SettingsCard({ title, children, id: anchor }: SettingsCardProps) {
  const id = useId();
  return (
    <section id={anchor} aria-labelledby={id} className={anchor ? "scroll-mt-4" : undefined}>
      <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
        <CardHeader>
          <h3 id={id} className="text-base font-semibold">
            {title}
          </h3>
        </CardHeader>
        <CardContent className="flex min-w-0 flex-col gap-4">{children}</CardContent>
      </Card>
    </section>
  );
}
