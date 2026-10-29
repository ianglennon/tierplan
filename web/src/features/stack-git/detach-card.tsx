// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ChevronRight } from "lucide-react";
import { useEffect, useId, useRef, useState } from "react";

import { StackGitDetachError, useDetachStackGit } from "@/api/stack-git";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Spinner } from "@/components/ui/spinner";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

interface DetachCardProps {
  envId: string;
  stackId: string;
  stackName: string;
  // The repository the stack is checked against, as the dialog names it.
  repoName: string;
  // The number of the revision that is deployed and the commit it was made from; absent for a stack not deployed yet.
  revision?: number;
  commit?: string;
}

// DetachCard is the Advanced section at the end of the Git tab (UI-SPEC "Git tab", D-25): a collapsible, closed by
// default, holding "Detach from git". Only an Admin of the environment sees it (git.manage); to anyone else it is not
// there, not disabled. Detach is not among the stack's header actions.
export function DetachCard({
  envId,
  stackId,
  stackName,
  repoName,
  revision,
  commit,
}: DetachCardProps) {
  const t = useT();
  const needs = useNeeds("git.manage", envId);
  const headingId = useId();
  const [open, setOpen] = useState(false);
  const [dialog, setDialog] = useState(false);

  // Another stack is another section: it starts closed.
  // biome-ignore lint/correctness/useExhaustiveDependencies: the ids are the trigger, not the body
  useEffect(() => {
    setOpen(false);
    setDialog(false);
  }, [envId, stackId]);

  if (!needs.allowed) return null;

  return (
    <section aria-labelledby={headingId}>
      <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-4">
        <h3 id={headingId} className="text-base font-semibold">
          <CollapsibleTrigger className="flex items-center gap-2 rounded-sm text-start focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11">
            <ChevronRight
              aria-hidden="true"
              className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
            />
            {t("stackGit.detach.advanced")}
          </CollapsibleTrigger>
        </h3>
        <CollapsibleContent>
          <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
            <CardHeader>
              <CardTitle>
                <h4 className="text-base font-semibold">{t("stackGit.detach.title")}</h4>
              </CardTitle>
              <CardDescription>{t("stackGit.detach.body", { name: stackName })}</CardDescription>
            </CardHeader>
            <CardContent>
              <Button
                type="button"
                variant="outline"
                className={`${TOUCH_HEIGHT} text-destructive`}
                onClick={() => setDialog(true)}
              >
                {t("stackGit.detach.action")}
              </Button>
            </CardContent>
          </Card>
        </CollapsibleContent>
      </Collapsible>
      <DetachDialog
        envId={envId}
        stackId={stackId}
        stackName={stackName}
        repoName={repoName}
        revision={revision}
        commit={commit}
        open={dialog}
        onOpenChange={setDialog}
      />
    </section>
  );
}

interface DetachDialogProps extends DetachCardProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

function refusalOf(e: unknown): "gone" | "permission" | "failed" {
  const status = e instanceof StackGitDetachError ? e.status : 0;
  const type = e instanceof StackGitDetachError ? e.type : undefined;
  if (status === 409 && type === "stack_not_git") return "gone";
  if (status === 403) return "permission";
  return "failed";
}

// DetachDialog is "Detach {name} from git?" (UI-SPEC "Git tab"): focus starts on "Keep tracking git"; "Detach from git" is
// destructive. The dialog stays open, with a refusal inside it, until the server answers. A stack that is not from git
// any more was detached already, and is treated as done.
export function DetachDialog({
  envId,
  stackId,
  stackName,
  repoName,
  revision,
  commit,
  open,
  onOpenChange,
}: DetachDialogProps) {
  const t = useT();
  const detach = useDetachStackGit(envId, stackId);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = detach.isPending;

  async function confirm() {
    if (inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await detach.mutateAsync();
      toastSuccess(t("stackGit.detach.done", { name: stackName }));
      onOpenChange(false);
    } catch (e) {
      const refusal = refusalOf(e);
      if (refusal === "gone") {
        onOpenChange(false);
      } else {
        setError(t(`stackGit.detach.${refusal}`));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(next: boolean) {
    if (!next && pending) return;
    if (!next) setError(null);
    onOpenChange(next);
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>
            {t("stackGit.detach.dialog.title", { name: stackName })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {revision !== undefined && commit
              ? t("stackGit.detach.dialog.body", {
                  revision,
                  sha: commit.slice(0, 7),
                  repo: repoName,
                })
              : t("stackGit.detach.dialog.bodyNoRevision", { repo: repoName })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens: the safe choice. */}
          <AlertDialogCancel disabled={pending} className={TOUCH_HEIGHT}>
            {t("stackGit.detach.dialog.cancel")}
          </AlertDialogCancel>
          <AlertDialogAction
            aria-disabled={pending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
            onClick={(event) => {
              // The dialog stays open until the answer, so a refusal can be shown in it.
              event.preventDefault();
              void confirm();
            }}
          >
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("stackGit.detach.dialog.pending")}
              </>
            ) : (
              t("stackGit.detach.dialog.confirm")
            )}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
