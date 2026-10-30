// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useMutation, useQueryClient } from "@tanstack/react-query";
import { type RefObject, useRef, useState } from "react";

import {
  AdoptClusterError,
  adoptCluster,
  type EnvironmentItem,
  environmentsQuery,
  summaryQuery,
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
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// What an inline message says, as a key under envsettings.adopt. A failure that
// has its own sentence is picked by the problem type; the rest is general.
type FailureKey = "noChange" | "inUse" | "failed";

interface AdoptClusterDialogProps {
  // The environment whose swarm was re-initialised. pendingClusterId is the
  // cluster its managers now report; the list has it only while the cluster is
  // changed.
  environment: Pick<EnvironmentItem, "id" | "name" | "pendingClusterId">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // What takes focus when the dialog closes. It is opened from state, with no
  // Radix trigger, which would otherwise leave focus on the page body.
  returnFocusRef?: RefObject<HTMLElement | null>;
}

// AdoptClusterDialog is "Adopt the new cluster?" (D-14): a plain dialog with the
// primary confirm, because adopting loses nothing and is recorded in the audit
// log. Confirming posts once. A failure keeps it open with an inline alert; one
// that says the change is gone also reads the environments again, behind the
// message, so the page underneath stops offering it.
export function AdoptClusterDialog(props: AdoptClusterDialogProps) {
  // Adopting needs environments.configure on the environment (D-07). Without it
  // there is nothing to confirm, and nothing the server would accept.
  const allowed = usePermission("environments.configure", props.environment.id);
  return allowed ? <AdoptClusterDialogBody {...props} /> : null;
}

function AdoptClusterDialogBody({
  environment,
  open,
  onOpenChange,
  returnFocusRef,
}: AdoptClusterDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [failure, setFailure] = useState<FailureKey | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  function refresh() {
    return Promise.all([
      queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
      queryClient.invalidateQueries({ queryKey: summaryQuery(environment.id).queryKey }),
    ]);
  }

  const adopt = useMutation({
    mutationFn: () => adoptCluster(environment.id),
    onSuccess: async () => {
      await refresh();
      onOpenChange(false);
      toastSuccess(t("envsettings.adopt.done", { name: environment.name }));
    },
    onError: (error) => {
      if (error instanceof AdoptClusterError && error.noChange) {
        setFailure("noChange");
        void refresh();
        return;
      }
      setFailure(error instanceof AdoptClusterError && error.inUse ? "inUse" : "failed");
    },
    onSettled: () => {
      inFlight.current = false;
    },
  });

  function change(next: boolean) {
    if (next || adopt.isPending) return;
    setFailure(null);
    adopt.reset();
    onOpenChange(false);
  }

  const id = environment.pendingClusterId;
  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent
        showCloseButton={false}
        onCloseAutoFocus={(event) => {
          if (!returnFocusRef?.current) return;
          event.preventDefault();
          returnFocusRef.current.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("envsettings.adopt.title")}</DialogTitle>
          <DialogDescription>
            {id
              ? t("envsettings.adopt.body", { name: environment.name, id })
              : t("envsettings.adopt.bodyNoId", { name: environment.name })}
          </DialogDescription>
        </DialogHeader>
        {failure ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{t(`envsettings.adopt.${failure}`)}</AlertDescription>
          </Alert>
        ) : null}
        <DialogFooter>
          <DialogClose asChild>
            <Button
              type="button"
              variant="outline"
              disabled={adopt.isPending}
              className={TOUCH_HEIGHT}
            >
              {t("envsettings.adopt.cancel")}
            </Button>
          </DialogClose>
          <Button
            type="button"
            disabled={adopt.isPending}
            className={TOUCH_HEIGHT}
            onClick={() => {
              if (inFlight.current) return;
              inFlight.current = true;
              setFailure(null);
              adopt.mutate();
            }}
          >
            {adopt.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("envsettings.adopt.pending")}
              </>
            ) : (
              t("envsettings.adopt.confirm")
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
