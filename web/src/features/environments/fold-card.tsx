// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";

import {
  type EnvironmentItem,
  environmentsQuery,
  FoldLocalError,
  foldLocal,
  refreshAfterFold,
} from "@/api/environments";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type FailureKey = "notOffered" | "failed";

interface FoldCardProps {
  // The environment whose settings page this is: the swarm, or the local host's own.
  env: EnvironmentItem;
  // Called with the swarm environment's ID once the fold went through, so the page of
  // the environment that was just retired can move to the swarm's.
  onFolded?: (swarmEnvId: string) => void;
  // Where focus goes when the dialog closes and the card's own button is gone, which is
  // what a fold does: the offer ends, and with it the card.
  focusFallback?: () => HTMLElement | null;
}

// FoldCard is the Local host card (D-48): this Bayplan host is a manager of a swarm that
// is enrolled through agents, so it can be reached through the Docker socket and needs no
// agent. "Fold into {swarm}" opens a plain dialog with the primary confirm, because
// nothing on the host changes and the fold is recorded in the audit log. Confirming posts
// once. A failure keeps the dialog open with an inline alert. It is rendered only while
// the server offers the fold, on the swarm's page and on the local host's own.
//
// Hiding the card is not access control; the server answers 403 without environments.manage.
export function FoldCard({ env, onFolded, focusFallback }: FoldCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const environments = useQuery(environmentsQuery);
  const [open, setOpen] = useState(false);
  const [failure, setFailure] = useState<FailureKey | null>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  // The swarm the host would be folded into: this environment, or for the local host's
  // own page the agent environment that offers it.
  const swarm =
    env.kind === "agent"
      ? env
      : environments.data?.items?.find((e) => e.kind === "agent" && e.foldOffered);

  const fold = useMutation({
    mutationFn: (swarmId: string) => foldLocal(swarmId),
    onSuccess: async (_data, swarmId) => {
      await refreshAfterFold(queryClient, swarmId);
      setOpen(false);
      toastSuccess(t("envsettings.fold.done", { swarm: swarm?.name ?? "" }));
      onFolded?.(swarmId);
    },
    onError: (error, swarmId) => {
      if (error instanceof FoldLocalError && error.notOffered) {
        setFailure("notOffered");
        // What is offered has changed, so the page underneath stops offering it.
        void refreshAfterFold(queryClient, swarmId);
        return;
      }
      setFailure("failed");
    },
    onSettled: () => {
      inFlight.current = false;
    },
  });

  if (!env.foldOffered || !swarm) return null;

  function change(next: boolean) {
    if (next) {
      setOpen(true);
      return;
    }
    if (fold.isPending) return;
    setFailure(null);
    fold.reset();
    setOpen(false);
  }

  const name = swarm.name;
  return (
    <SettingsCard title={t("envsettings.fold.title")} id="fold">
      <Alert>
        <AlertDescription>{t("envsettings.fold.alert", { swarm: name })}</AlertDescription>
      </Alert>
      <div>
        <Button
          ref={trigger}
          type="button"
          className={`${TOUCH_HEIGHT} max-md:w-full`}
          onClick={() => setOpen(true)}
        >
          {t("envsettings.fold.button", { swarm: name })}
        </Button>
      </div>
      <Dialog open={open} onOpenChange={change}>
        <DialogContent
          showCloseButton={false}
          onCloseAutoFocus={(event) => {
            const target = trigger.current?.isConnected ? trigger.current : focusFallback?.();
            if (!target) return;
            event.preventDefault();
            target.focus();
          }}
        >
          <DialogHeader>
            <DialogTitle>{t("envsettings.fold.dialogTitle", { swarm: name })}</DialogTitle>
            <DialogDescription>{t("envsettings.fold.body")}</DialogDescription>
          </DialogHeader>
          {failure ? (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{t(`envsettings.fold.${failure}`)}</AlertDescription>
            </Alert>
          ) : null}
          <DialogFooter>
            <DialogClose asChild>
              <Button
                type="button"
                variant="outline"
                disabled={fold.isPending}
                className={TOUCH_HEIGHT}
              >
                {t("envsettings.fold.cancel")}
              </Button>
            </DialogClose>
            <Button
              type="button"
              disabled={fold.isPending}
              className={TOUCH_HEIGHT}
              onClick={() => {
                if (inFlight.current) return;
                inFlight.current = true;
                setFailure(null);
                fold.mutate(swarm.id);
              }}
            >
              {fold.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("envsettings.fold.pending")}
                </>
              ) : (
                t("envsettings.fold.confirm", { swarm: name })
              )}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </SettingsCard>
  );
}
