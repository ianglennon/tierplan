// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo } from "react";

import { environmentsQuery } from "@/api/environments";
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
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
export const TOUCH_HEIGHT = "h-11 md:h-8";

// The Scope select's value for the instance layer; any other value is an environment's ID.
export const INSTANCE = "instance";

interface SheetFrameProps {
  title: string;
  description: string;
  onOpenChange: (open: boolean) => void;
  footer: ReactNode;
  children: ReactNode;
}

// The sheet's frame: 560px at most, the full width of a phone, and a body that scrolls under a
// header and a footer that stay. Two sheets can be open at once (a credential added from a
// repository), so the description's id is its own.
export function SheetFrame({
  title,
  description,
  onOpenChange,
  footer,
  children,
}: SheetFrameProps) {
  const descriptionId = useId();
  return (
    <Sheet open onOpenChange={onOpenChange}>
      <SheetContent
        className="w-full gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-140"
        aria-describedby={descriptionId}
      >
        <SheetHeader className="border-b pe-12">
          <SheetTitle className="break-words">{title}</SheetTitle>
          <SheetDescription id={descriptionId}>{description}</SheetDescription>
        </SheetHeader>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 py-4">{children}</div>
        <SheetFooter className="sticky bottom-0 mt-0 flex-row justify-end border-t bg-popover">
          {footer}
        </SheetFooter>
      </SheetContent>
    </Sheet>
  );
}

// SkeletonFields stands in for the form while an existing item loads.
export function SkeletonFields() {
  return (
    <div className="flex flex-col gap-3" aria-busy="true">
      {Array.from({ length: 7 }, (_, i) => (
        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
        <Skeleton key={i} className="h-5 w-full" aria-hidden="true" />
      ))}
    </div>
  );
}

interface GoneStateProps {
  title: string;
  body: string;
  back: string;
  onBack: () => void;
}

// GoneState is what a sheet shows for a row that was deleted while it was open.
export function GoneState({ title, body, back, onBack }: GoneStateProps) {
  return (
    <div role="status" className="flex flex-col items-start gap-3">
      <h3 className="font-heading text-base font-medium">{title}</h3>
      <p className="text-sm text-muted-foreground">{body}</p>
      <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onBack}>
        {back}
      </Button>
    </div>
  );
}

interface LoadErrorProps {
  title: string;
  body: string;
  retry: string;
  retrying: boolean;
  onRetry: () => void;
}

// LoadError is what a sheet shows when the list it reads from could not be read.
export function LoadError({ title, body, retry, retrying, onRetry }: LoadErrorProps) {
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription className="flex flex-col items-start gap-2">
        <span className="font-medium">{title}</span>
        <span>{body}</span>
        <Button variant="outline" size="sm" disabled={retrying} onClick={onRetry}>
          {retrying ? <Spinner aria-hidden="true" /> : null}
          {retry}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

interface DiscardDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // "repoSheet" or "credSheet": the copy of the sheet the dialog guards.
  copy: "repoSheet" | "credSheet";
  onDiscard: () => void;
}

// DiscardDialog is the unsaved-changes guard: focus starts on the safe button, "Keep editing".
export function DiscardDialog({ open, onOpenChange, copy, onDiscard }: DiscardDialogProps) {
  const t = useT();
  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          <AlertDialogTitle>{t(`git.${copy}.discard.title`)}</AlertDialogTitle>
          <AlertDialogDescription>{t(`git.${copy}.discard.body`)}</AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel autoFocus>{t(`git.${copy}.discard.keep`)}</AlertDialogCancel>
          <AlertDialogAction
            onClick={() => {
              onOpenChange(false);
              onDiscard();
            }}
          >
            {t(`git.${copy}.discard.discard`)}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

// A place a repository or credential can be added: the instance layer, or one environment.
export interface ScopeChoice {
  value: string;
  label: string;
}

// useScopeChoices lists where the signed-in user may add a repository or credential (D-01, D-02):
// the instance layer for a holder of git.instance, and each environment where they hold
// git.manage. copy names the sheet whose "instance" wording to use. The environments come from the
// list the switcher already reads. ready is false until that list has arrived, so a default is not
// chosen from half the options.
export function useScopeChoices(copy: "repoSheet" | "credSheet"): {
  choices: ScopeChoice[];
  ready: boolean;
} {
  const t = useT();
  const me = useMe();
  const environments = useQuery(environmentsQuery);
  return useMemo(() => {
    const choices: ScopeChoice[] = [];
    if (hasPermission(me.data, "git.instance")) {
      choices.push({ value: INSTANCE, label: t(`git.${copy}.scope.instance`) });
    }
    const named = (environments.data?.items ?? [])
      .filter((e) => hasPermission(me.data, "git.manage", e.id))
      .map((e) => ({ value: e.id, label: e.name }))
      .sort((a, b) => a.label.localeCompare(b.label));
    return {
      choices: [...choices, ...named],
      ready: me.isSuccess && (environments.isSuccess || environments.isError),
    };
  }, [
    t,
    copy,
    me.data,
    me.isSuccess,
    environments.data,
    environments.isSuccess,
    environments.isError,
  ]);
}

// The environment a scope value names, or undefined for the instance layer.
export function envOfScope(scope: string): string | undefined {
  return scope === INSTANCE ? undefined : scope;
}
