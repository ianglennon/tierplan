// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter, useRouterState } from "@tanstack/react-router";
import { ChevronDown } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import { agentsQuery, removeEnvironment } from "@/api/agents";
import { type EnvironmentItem, environmentsQuery, recentQuery } from "@/api/environments";
import { CopyableCode } from "@/components/copyable-code";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { localHostQuery } from "@/features/environments/local-host-panel";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface RemoveEnvironmentCardProps {
  env: EnvironmentItem;
}

// RemoveEnvironmentCard is the last section of an environment's settings page, with
// the anchor #remove that the "Remove environment" actions elsewhere link to (D-30).
// Its alert-dialog has focus on Cancel and a destructive confirm. For an agent
// environment it also offers "Also uninstall the agent from the hosts", checked by
// default when any agent is online, and always the manual cleanup command, because
// a host that is offline cannot be told. The local socket host is not an agent: it
// keeps Phase 1's "Remove host" wording and has neither the box nor the command.
// The page shows the card only to someone holding environments.delete; the server
// checks again.
export function RemoveEnvironmentCard({ env }: RemoveEnvironmentCardProps) {
  const t = useT();
  const router = useRouter();
  const queryClient = useQueryClient();
  const isLocal = env.kind === "local";
  const [open, setOpen] = useState(false);
  // Null until the person has touched the box, so that it follows who is online.
  const [choice, setChoice] = useState<boolean | null>(null);
  const [failed, setFailed] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const checkboxId = useId();
  const helperId = useId();

  // /settings/environments/{id}#remove scrolls to this card once it is on the page.
  const hash = useRouterState({ select: (s) => s.location.hash });
  const wanted = hash.replace(/^#/, "") === "remove";
  useEffect(() => {
    if (wanted) document.getElementById("remove")?.scrollIntoView({ block: "start" });
  }, [wanted]);

  const agents = useQuery({ ...agentsQuery(env.id), enabled: !isLocal });
  const active = (agents.data?.items ?? []).filter((a) => a.status === "active");
  const online = active.filter((a) => a.connected).length;
  const total = active.length;
  const known = !isLocal && agents.isSuccess;
  const noneOnline = known && online === 0;
  // What the box shows and what the request sends. With nobody online it is off
  // and cannot be turned on.
  const uninstall = !isLocal && !noneOnline && (choice ?? (known && online > 0));
  const cleanup = agents.data?.manualCleanup;

  const remove = useMutation({
    mutationFn: () => removeEnvironment(env.id, uninstall),
    onSuccess: async () => {
      setOpen(false);
      toastSuccess(t("envsettings.remove.done", { name: env.name }));
      // Leave the page first: nothing cached for the environment may be read again.
      await router.navigate({ to: "/environments" });
      for (const key of [
        ["env", env.id],
        ["agents", env.id],
        ["env-token", env.id],
      ]) {
        queryClient.removeQueries({ queryKey: key });
      }
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
        queryClient.invalidateQueries({ queryKey: recentQuery.queryKey }),
        queryClient.invalidateQueries({ queryKey: localHostQuery.queryKey }),
      ]);
    },
    onError: () => setFailed(true),
  });

  function changeOpen(next: boolean) {
    if (!next && remove.isPending) return;
    setOpen(next);
    if (next) {
      setChoice(null);
      setFailed(false);
      remove.reset();
    }
  }

  let helper: string;
  if (agents.isError) helper = t("envsettings.remove.helperUnknown");
  else if (!known) helper = t("envsettings.remove.helperLoading");
  else if (online === 0) helper = t("envsettings.remove.helperNone");
  else if (online === total) helper = t("envsettings.remove.helperAll", { total });
  else helper = t("envsettings.remove.helperSome", { online, total });

  return (
    <SettingsCard id="remove" title={t("envsettings.sections.remove")}>
      <p className="text-sm text-muted-foreground">
        {t(isLocal ? "envsettings.remove.cardBodyLocal" : "envsettings.remove.cardBody")}
      </p>
      <div>
        <Button
          ref={trigger}
          type="button"
          className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 max-md:w-full`}
          onClick={() => changeOpen(true)}
        >
          {t(isLocal ? "envsettings.remove.buttonLocal" : "envsettings.remove.button")}
        </Button>
      </div>

      <AlertDialog open={open} onOpenChange={changeOpen}>
        <AlertDialogContent
          // Wide enough for a cleanup command, which scrolls inside its block.
          className="data-[size=default]:sm:max-w-lg"
          onCloseAutoFocus={(event) => {
            // Back to the button that opened it. After a removal the page is gone
            // and there is nothing to focus.
            event.preventDefault();
            if (trigger.current?.isConnected) trigger.current.focus();
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{t("envsettings.remove.title", { name: env.name })}</AlertDialogTitle>
            <AlertDialogDescription>
              {t(isLocal ? "settings.removeConfirm.body" : "envsettings.remove.body")}
            </AlertDialogDescription>
          </AlertDialogHeader>

          {isLocal ? null : (
            // min-w-0: a grid item is as wide as its widest line unless told it may shrink.
            <div className="flex min-w-0 flex-col gap-3">
              <div className="flex items-start gap-3">
                <Checkbox
                  id={checkboxId}
                  // A 16px box is too small to touch: the pointer area grows under 768px.
                  className="relative max-md:after:absolute max-md:after:-inset-3.5"
                  checked={uninstall}
                  disabled={noneOnline || remove.isPending}
                  aria-describedby={helperId}
                  onCheckedChange={(v) => setChoice(v === true)}
                />
                <div className="flex min-w-0 flex-col gap-1">
                  <Label htmlFor={checkboxId} className="leading-snug">
                    {t("envsettings.remove.uninstall")}
                  </Label>
                  <p id={helperId} className="text-sm text-muted-foreground">
                    {helper}
                  </p>
                </div>
              </div>
              <ManualCleanup env={env} swarm={cleanup?.swarm} standalone={cleanup?.standalone} />
            </div>
          )}

          {remove.isPending && uninstall ? (
            <p role="status" className="text-sm text-muted-foreground">
              {t("envsettings.remove.waiting")}
            </p>
          ) : null}
          {failed ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{t("envsettings.remove.failed")}</AlertDescription>
            </Alert>
          ) : null}

          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel disabled={remove.isPending} className={TOUCH_HEIGHT}>
              {t("envsettings.remove.cancel")}
            </AlertDialogCancel>
            <Button
              type="button"
              disabled={remove.isPending}
              className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
              onClick={() => {
                // Radix would close the dialog on a click; it stays open until the
                // request answers, which can take up to 30 seconds.
                if (remove.isPending) return;
                setFailed(false);
                remove.mutate();
              }}
            >
              {remove.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("envsettings.remove.pending")}
                </>
              ) : (
                t(isLocal ? "settings.removeConfirm.confirm" : "envsettings.remove.confirm")
              )}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </SettingsCard>
  );
}

interface ManualCleanupProps {
  env: EnvironmentItem;
  swarm?: string;
  standalone?: string;
}

// ManualCleanup is the collapsible "Manual cleanup command": what to run on each
// host whose agent was not uninstalled. It is always there, whatever the box says.
// The swarm and standalone commands are both shown while the environment's type is
// not known (no agent has enrolled), otherwise the one that fits.
function ManualCleanup({ env, swarm, standalone }: ManualCleanupProps) {
  const t = useT();
  const showSwarm = env.envType !== "standalone" && swarm;
  const showStandalone = env.envType !== "swarm" && standalone;
  const both = Boolean(showSwarm && showStandalone);
  return (
    <Collapsible className="flex min-w-0 flex-col gap-2">
      <CollapsibleTrigger asChild>
        <Button type="button" variant="ghost" size="sm" className="group -ms-2 w-fit max-md:h-11">
          <ChevronDown
            aria-hidden="true"
            className="transition-transform group-data-[state=open]:rotate-180"
          />
          {t("envsettings.remove.manualTrigger")}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="flex min-w-0 flex-col gap-3">
        <p className="text-sm">{t("envsettings.remove.manualBody")}</p>
        {showSwarm ? (
          <div className="flex min-w-0 flex-col gap-1">
            {both ? (
              <span className="text-xs font-semibold text-muted-foreground">
                {t("envsettings.remove.manualSwarm")}
              </span>
            ) : null}
            <CopyableCode code={swarm} label={t("envsettings.remove.manualSwarmLabel")} />
          </div>
        ) : null}
        {showStandalone ? (
          <div className="flex min-w-0 flex-col gap-1">
            {both ? (
              <span className="text-xs font-semibold text-muted-foreground">
                {t("envsettings.remove.manualStandalone")}
              </span>
            ) : null}
            <CopyableCode code={standalone} label={t("envsettings.remove.manualStandaloneLabel")} />
          </div>
        ) : null}
      </CollapsibleContent>
    </Collapsible>
  );
}
