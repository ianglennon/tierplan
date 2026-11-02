// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useRouter } from "@tanstack/react-router";
import { Server } from "lucide-react";
import { type RefObject, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { api } from "@/api/client";
import { type EnvironmentItem, environmentsQuery } from "@/api/environments";
import { readProblem } from "@/api/problem";
import { StatusDot, useHealthLabel } from "@/components/status-dot";
import { TypeBadge } from "@/components/type-badge";
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
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Item, ItemActions, ItemContent, ItemGroup, ItemTitle } from "@/components/ui/item";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { LocalHostPanel, localHostQuery } from "@/features/environments/local-host-panel";
import { UnknownAgentsCard } from "@/features/environments/unknown-agents-card";
import { hasPermission, useMe } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The longest environment name the server accepts, counted in characters the
// way it counts them (runes), not UTF-16 units.
const MAX_NAME_LENGTH = 64;
const chars = (s: string) => [...s].length;

// Validation here is UX only: the server checks the name again and answers 422
// invalid_name. Messages are i18n keys, never literals (D-65).
const KEY = {
  required: "settings.rename.errors.required",
  tooLong: "settings.rename.errors.tooLong",
  invalid: "settings.rename.errors.invalid",
  server: "common.mutationFailed",
  unreachable: "common.errors.unreachable",
} as const;

const renameSchema = z.object({
  name: z
    .string()
    .trim()
    .min(1, { error: KEY.required })
    .refine((v) => chars(v) <= MAX_NAME_LENGTH, { error: KEY.tooLong }),
});

type RenameValues = z.input<typeof renameSchema>;

interface RenameFormProps {
  environment: EnvironmentItem;
  onDone: () => void;
  onPendingChange: (pending: boolean) => void;
}

// RenameForm lives inside the dialog content, so closing the dialog unmounts
// it. While the request is in flight the field and both buttons are disabled
// and a second submit is ignored (UI-SPEC "Submit pending").
function RenameForm({ environment, onDone, onPendingChange }: RenameFormProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const [pending, setPending] = useState(false);
  const [formError, setFormError] = useState<string | null>(null);
  const submitting = useRef(false);

  const form = useForm<RenameValues>({
    resolver: zodResolver(renameSchema),
    defaultValues: { name: environment.name },
    mode: "onSubmit",
  });
  const message = form.formState.errors.name?.message;
  const nameError = typeof message === "string" ? t(message) : undefined;

  function setBusy(busy: boolean) {
    submitting.current = busy;
    setPending(busy);
    onPendingChange(busy);
  }

  async function submit(values: RenameValues) {
    if (submitting.current) return;
    const name = values.name.trim();
    // Nothing to change: no request, so no audit entry for a no-op.
    if (name === environment.name) {
      onDone();
      return;
    }
    setBusy(true);
    setFormError(null);
    try {
      const { error, response } = await api.PATCH("/api/env/{envId}", {
        params: { path: { envId: environment.id } },
        body: { name },
      });
      if (response.ok) {
        // The name is also in the environment's own summary, which the
        // dashboard header reads.
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
          queryClient.invalidateQueries({ queryKey: ["env", environment.id] }),
        ]);
        onDone();
        toastSuccess(t("settings.saved"));
        return;
      }
      const problem = readProblem(error, response);
      if (problem.status === 422) {
        form.setError("name", { type: "server", message: KEY.invalid });
        form.setFocus("name");
        return;
      }
      setFormError(KEY.server);
    } catch {
      setFormError(KEY.unreachable);
    } finally {
      setBusy(false);
    }
  }

  const name = form.register("name");
  const errorId = "rename-environment-name-error";
  const helperId = "rename-environment-name-helper";

  return (
    <form noValidate onSubmit={form.handleSubmit(submit)} className="flex flex-col gap-4">
      {formError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{t(formError)}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="contents">
        <FieldGroup className="gap-4">
          <Field data-invalid={nameError ? true : undefined}>
            <FieldLabel htmlFor="rename-environment-name">
              {t("settings.rename.name.label")}
            </FieldLabel>
            <Input
              id="rename-environment-name"
              {...name}
              autoComplete="off"
              aria-invalid={Boolean(nameError)}
              aria-describedby={nameError ? `${helperId} ${errorId}` : helperId}
              className={TOUCH_HEIGHT}
            />
            <FieldDescription id={helperId}>{t("settings.rename.name.helper")}</FieldDescription>
            {nameError ? <FieldError id={errorId}>{nameError}</FieldError> : null}
          </Field>
        </FieldGroup>
        <DialogFooter>
          <Button type="button" variant="outline" className={TOUCH_HEIGHT} onClick={onDone}>
            {t("settings.rename.cancel")}
          </Button>
          <Button type="submit" className={TOUCH_HEIGHT}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("settings.rename.pending")}
              </>
            ) : (
              t("settings.rename.submit")
            )}
          </Button>
        </DialogFooter>
      </fieldset>
    </form>
  );
}

export interface RenameDialogProps {
  environment: EnvironmentItem;
  onClose: () => void;
  returnFocusRef: RefObject<HTMLElement | null>;
}

// RenameDialog is "Rename environment" (D-20). It cannot be dismissed while the
// request is in flight, and focus goes back to the row's Rename button.
export function RenameDialog({ environment, onClose, returnFocusRef }: RenameDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);
  return (
    <Dialog open onOpenChange={(open) => (open || pending ? undefined : onClose())}>
      <DialogContent
        showCloseButton={false}
        onCloseAutoFocus={(event) => {
          if (!returnFocusRef.current) return;
          event.preventDefault();
          returnFocusRef.current.focus();
        }}
      >
        <DialogHeader>
          <DialogTitle>{t("settings.rename.title")}</DialogTitle>
          <DialogDescription>{t("settings.rename.description")}</DialogDescription>
        </DialogHeader>
        <RenameForm environment={environment} onDone={onClose} onPendingChange={setPending} />
      </DialogContent>
    </Dialog>
  );
}

// EnvironmentRow is one environment as an `item`: name, type, health, engine
// version and, for the host TierPlan itself runs on, the own-instance badge.
function EnvironmentRow({
  environment,
  canRemove,
  onRename,
  onRemove,
}: {
  environment: EnvironmentItem;
  // Removing the host is environments.delete, which an env Admin does not hold.
  canRemove: boolean;
  onRename: (trigger: HTMLElement) => void;
  onRemove: (trigger: HTMLElement) => void;
}) {
  const t = useT();
  const healthLabel = useHealthLabel();
  // Phase 1's Rename and Remove host belong to the local host. An agent
  // environment is managed on its own page.
  const isLocal = environment.kind === "local";
  return (
    <Item variant="outline" role="listitem" className="max-md:gap-3">
      <ItemContent>
        <ItemTitle className="w-full flex-wrap">
          {/* Every environment has a page of its own (D-38). */}
          <Link
            to="/settings/environments/$envId"
            params={{ envId: environment.id }}
            className="min-w-0 break-words text-accent-ink underline-offset-4 hover:underline focus-visible:rounded-sm focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
            title={environment.name}
          >
            {environment.name}
          </Link>
          <TypeBadge
            envType={environment.envType}
            type={environment.type}
            isLocalSocket={environment.isLocalSocket}
          />
          {environment.isSelfHost ? (
            <Badge variant="outline" className="shrink-0">
              {t("dashboard.selfBadge")}
            </Badge>
          ) : null}
        </ItemTitle>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <StatusDot state={environment.status.state} label={healthLabel(environment.status)} />
          <span>
            {t("settings.environments.engine")}{" "}
            <span className="font-mono">
              {environment.engine.version || t("settings.environments.engineMissing")}
            </span>
          </span>
        </div>
      </ItemContent>
      <ItemActions className="max-md:w-full">
        {isLocal ? (
          <>
            <Button
              type="button"
              variant="ghost"
              className={`${TOUCH_HEIGHT} max-md:flex-1`}
              aria-label={t("settings.environments.renameAria", { name: environment.name })}
              onClick={(e) => onRename(e.currentTarget)}
            >
              {t("settings.environments.rename")}
            </Button>
            {canRemove ? (
              <Button
                type="button"
                variant="outline"
                className={`${TOUCH_HEIGHT} border-destructive/40 text-status-offline-text hover:text-status-offline-text max-md:flex-1`}
                aria-label={t("settings.environments.removeAria", { name: environment.name })}
                onClick={(e) => onRemove(e.currentTarget)}
              >
                {t("settings.environments.remove")}
              </Button>
            ) : null}
          </>
        ) : (
          <Button asChild variant="outline" className={`${TOUCH_HEIGHT} max-md:flex-1`}>
            <Link
              to="/settings/environments/$envId"
              params={{ envId: environment.id }}
              aria-label={t("envsettings.list.manageAria", { name: environment.name })}
            >
              {t("envsettings.list.manage")}
            </Link>
          </Button>
        )}
      </ItemActions>
    </Item>
  );
}

// EnvironmentsSettings is Settings > Environments (D-20, D-60): the rows with
// Rename and Remove host, and, while the local host is not an environment, the
// same opt-in the wizard offers. The rows are the environments the user
// administers (environments.configure on each, D-07); Remove host and the opt-in
// are for those who hold environments.delete and environments.create, which an
// env Admin does not. Every change is audited by the server.
export function EnvironmentsSettings() {
  const t = useT();
  const router = useRouter();
  const queryClient = useQueryClient();
  const { data: me } = useMe();
  const canCreate = hasPermission(me, "environments.create");
  const canDelete = hasPermission(me, "environments.delete");
  const environments = useQuery(environmentsQuery);
  const [renaming, setRenaming] = useState<EnvironmentItem | null>(null);
  const [removing, setRemoving] = useState<EnvironmentItem | null>(null);
  // The button that opened a dialog, so closing it puts focus back there.
  const trigger = useRef<HTMLElement | null>(null);
  const listRef = useRef<HTMLDivElement>(null);

  const remove = useMutation({
    mutationFn: async (environment: EnvironmentItem) => {
      const { response } = await api.DELETE("/api/environments/{envId}", {
        params: { path: { envId: environment.id } },
      });
      // 404 means it is already gone, which is what was asked for.
      if (response.ok || response.status === 404) return environment;
      throw new Error(`DELETE /api/environments/${environment.id} failed with ${response.status}`);
    },
    onSuccess: async (environment) => {
      setRemoving(null);
      // Nothing cached for the environment is worth keeping, and nothing may
      // keep reading it.
      queryClient.removeQueries({ queryKey: ["env", environment.id] });
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: environmentsQuery.queryKey }),
        queryClient.invalidateQueries({ queryKey: localHostQuery.queryKey }),
      ]);
      if (router.state.location.pathname.startsWith(`/env/${environment.id}`)) {
        await router.navigate({ to: "/" });
      }
    },
    onError: () => toastError(t("common.mutationFailed")),
  });

  if (environments.isPending) {
    return (
      <div className="flex flex-col gap-3" aria-busy="true">
        <span className="sr-only" role="status">
          {t("settings.environments.loading")}
        </span>
        <Skeleton className="h-16 w-full" />
      </div>
    );
  }

  if (environments.isError) {
    return (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("settings.environments.loadFailed")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={environments.isFetching}
            onClick={() => void environments.refetch()}
          >
            {t("settings.environments.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const all = environments.data.items ?? [];
  // The server lists what the user may read; this narrows it to what they administer.
  const items = all.filter((e) => hasPermission(me, "environments.configure", e.id));
  // Only the local host exists as an environment kind for now, so the opt-in is
  // for when none of the environments is it, whoever may see it.
  const hasLocal = all.some((e) => e.kind === "local");

  return (
    <div ref={listRef} tabIndex={-1} className="flex flex-col gap-6 outline-none">
      {/* After a restore: the agents installed after the backup (D-31). Instance
          admins only, and only while there are any. */}
      <UnknownAgentsCard />
      {items.length > 0 ? (
        <ItemGroup className="gap-3">
          {items.map((environment) => (
            <EnvironmentRow
              key={environment.id}
              environment={environment}
              canRemove={canDelete}
              onRename={(button) => {
                trigger.current = button;
                setRenaming(environment);
              }}
              onRemove={(button) => {
                trigger.current = button;
                setRemoving(environment);
              }}
            />
          ))}
        </ItemGroup>
      ) : (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Server aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>{t("env.empty.heading")}</EmptyTitle>
            <EmptyDescription>{t("env.empty.body")}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {hasLocal || !canCreate ? null : (
        <Card className="py-6">
          <CardContent className="px-6">
            {/* No onDone: in Settings the panel invalidates what it changed and
                has nothing to skip. It renders nothing once the host is managed. */}
            <LocalHostPanel mode="settings" />
          </CardContent>
        </Card>
      )}

      {renaming ? (
        <RenameDialog
          key={renaming.id}
          environment={renaming}
          returnFocusRef={trigger}
          onClose={() => setRenaming(null)}
        />
      ) : null}

      <AlertDialog
        open={removing !== null}
        onOpenChange={(open) => {
          if (!open && !remove.isPending) setRemoving(null);
        }}
      >
        <AlertDialogContent
          onCloseAutoFocus={(event) => {
            // Focus goes back to the button that opened the dialog. After a
            // removal that button is gone, and the list takes focus instead.
            event.preventDefault();
            const back = trigger.current?.isConnected ? trigger.current : listRef.current;
            back?.focus();
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>
              {t("settings.removeConfirm.title", { name: removing?.name ?? "" })}
            </AlertDialogTitle>
            <AlertDialogDescription>{t("settings.removeConfirm.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            {/* Radix focuses the Cancel button when the dialog opens. */}
            <AlertDialogCancel disabled={remove.isPending} className={TOUCH_HEIGHT}>
              {t("settings.removeConfirm.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              disabled={remove.isPending}
              className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
              onClick={(event) => {
                // Keep the dialog open until the request answers.
                event.preventDefault();
                if (removing && !remove.isPending) remove.mutate(removing);
              }}
            >
              {remove.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("settings.removeConfirm.pending")}
                </>
              ) : (
                t("settings.removeConfirm.confirm")
              )}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
