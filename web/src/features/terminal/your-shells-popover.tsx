// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useState } from "react";

import { myShellsQuery, ShellEndError, useEndMyShell } from "@/api/shells";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Spinner } from "@/components/ui/spinner";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastInfo, toastSuccess } from "@/lib/toast";

interface YourShellsPopoverProps {
  envId: string;
  // The container whose Shell tab this is: the shells listed are the ones the person has
  // open in it.
  containerId: string;
}

// YourShellsPopover is "Your shells ({n})" on the Shell tab (D-31, D-37): the shells the
// signed-in user has open in this container, in this tab and others, each with when it
// started and "End session". It is shown only when there is at least one, and lists
// nobody else's whatever the person's role. The list is read again every 15 seconds and
// when the window is focused. The popover is w-80 with a height of at most 320px, and
// scrolls inside.
export function YourShellsPopover({ envId, containerId }: YourShellsPopoverProps) {
  const t = useT();
  const needs = useNeeds("containers.exec", envId);
  const mine = useQuery(myShellsQuery(envId, containerId, needs.allowed));
  const end = useEndMyShell(envId);
  const [ending, setEnding] = useState<string | null>(null);
  const items = mine.data ?? [];
  if (items.length === 0) return null;

  async function endOne(id: string) {
    if (ending !== null) return;
    setEnding(id);
    try {
      await end.mutateAsync({ id });
      toastSuccess(t("activeshells.yours.ended"));
    } catch (e) {
      if (e instanceof ShellEndError && e.type === "shell_not_found") {
        toastInfo(t("activeshells.yours.gone"));
      } else {
        toastError(t("activeshells.yours.failed"));
      }
    } finally {
      setEnding(null);
    }
  }

  return (
    <Popover>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" size="sm" className="max-md:h-11">
          {t("activeshells.yours.trigger", { count: items.length })}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="max-h-80 w-80 overflow-y-auto">
        <h4 className="text-sm font-semibold">{t("activeshells.yours.heading")}</h4>
        <ul aria-label={t("activeshells.yours.label")} className="flex flex-col gap-2">
          {items.map((s) => (
            <li key={s.id} className="flex min-w-0 items-center justify-between gap-2">
              <span className="flex min-w-0 flex-col">
                <span className="truncate font-mono text-xs" title={s.container}>
                  {s.container}
                </span>
                <span className="text-xs text-muted-foreground">
                  <RelativeTime value={s.startedAt} />
                </span>
              </span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="shrink-0 max-md:h-11"
                aria-label={t("activeshells.yours.endFor", { container: s.container })}
                disabled={ending !== null}
                onClick={() => void endOne(s.id)}
              >
                {ending === s.id ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("activeshells.yours.ending")}
                  </>
                ) : (
                  t("activeshells.yours.end")
                )}
              </Button>
            </li>
          ))}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
