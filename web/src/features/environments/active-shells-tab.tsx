// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { EllipsisVertical, Terminal } from "lucide-react";
import { useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { activeShellsQuery, ShellEndError, type ShellItem, useEndShell } from "@/api/shells";
import { RelativeTime } from "@/components/relative-time";
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { SettingsCard } from "@/features/environments/settings-card";
import { TruncatedText } from "@/features/resources/resource-table";
import { useEnvIsSwarm } from "@/features/resources/swarm-lists";
import { useT } from "@/i18n/use-t";
import { toastInfo, toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Six skeleton rows hold the place of the table while the first poll loads.
const SKELETON_ROWS = 6;

// IdleCell is "{n} min", or "Active" when the shell has been quiet for under a minute.
function IdleCell({ seconds }: { seconds: number }) {
  const t = useT();
  const minutes = Math.floor(Math.max(seconds, 0) / 60);
  return (
    <span>
      {minutes < 1
        ? t("activeshells.idle.active")
        : t("activeshells.idle.minutes", { count: minutes })}
    </span>
  );
}

interface EndShellDialogProps {
  envId: string;
  // The shell to end, or null while the dialog is closed.
  shell: ShellItem | null;
  onClose: () => void;
}

// EndShellDialog is "End {user}'s shell?" (D-37): a destructive confirm, since what
// runs in the shell stops. Focus starts on Cancel, the dialog cannot be dismissed while
// the request is out, and a refusal stays inside it. A shell that closed on its own
// meanwhile is said so in a toast and the dialog closes: the refreshed list no longer
// has it.
function EndShellDialog({ envId, shell, onClose }: EndShellDialogProps) {
  const t = useT();
  const mutation = useEndShell(envId);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const user = shell?.user ?? "";

  async function end() {
    if (!shell || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await mutation.mutateAsync({ id: shell.id });
      toastSuccess(t("activeshells.end.done", { user }));
      onClose();
    } catch (e) {
      if (e instanceof ShellEndError && e.type === "shell_not_found") {
        toastInfo(t("activeshells.end.gone"));
        onClose();
      } else {
        setError(t("activeshells.end.failed"));
      }
    } finally {
      inFlight.current = false;
    }
  }

  function change(open: boolean) {
    if (open || mutation.isPending) return;
    setError(null);
    onClose();
  }

  return (
    <AlertDialog open={shell !== null} onOpenChange={change}>
      <AlertDialogContent className="sm:max-w-120">
        <AlertDialogHeader>
          {/* A long name wraps rather than running off the dialog. */}
          <AlertDialogTitle className="break-words">
            {t("activeshells.end.title", { user })}
          </AlertDialogTitle>
          <AlertDialogDescription className="break-words">
            {t("activeshells.end.body", { container: shell?.container ?? "" })}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <AlertDialogFooter>
          {/* Radix focuses the Cancel button when the dialog opens. */}
          <AlertDialogCancel disabled={mutation.isPending} className={TOUCH_HEIGHT}>
            {t("activeshells.end.cancel")}
          </AlertDialogCancel>
          <Button
            type="button"
            aria-disabled={mutation.isPending}
            className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90 aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
            onClick={() => void end()}
          >
            {mutation.isPending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("activeshells.end.pending")}
              </>
            ) : (
              t("activeshells.end.confirm")
            )}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function TableSkeleton({ swarm }: { swarm: boolean }) {
  const t = useT();
  return (
    <div aria-busy="true">
      <span role="status" className="sr-only">
        {t("envsettings.loading")}
      </span>
      <Table>
        <TableBody>
          {Array.from({ length: SKELETON_ROWS }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
            <TableRow key={i}>
              {Array.from({ length: swarm ? 6 : 5 }, (_, c) => (
                // biome-ignore lint/suspicious/noArrayIndexKey: placeholder cells have no identity
                <TableCell key={c}>
                  <Skeleton className="h-4 w-full" />
                </TableCell>
              ))}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

interface ActiveShellsTabProps {
  envId: string;
}

// ActiveShellsTab is Settings > Environments > {env} > Shells (D-37): every shell open on
// the environment, whoever opened it, with how long each has sat unused, and "End
// session…" on each row. The list is read again every 15 seconds and when the window
// is focused. It is for someone who holds shells.manage on the environment; the server
// checks that again on both routes.
export function ActiveShellsTab({ envId }: ActiveShellsTabProps) {
  const t = useT();
  const environments = useQuery(environmentsQuery);
  const name = environments.data?.items?.find((e) => e.id === envId)?.name ?? "";
  const swarm = useEnvIsSwarm(envId);
  const shells = useQuery(activeShellsQuery(envId));
  const [ending, setEnding] = useState<ShellItem | null>(null);
  const items = shells.data ?? [];

  return (
    <SettingsCard title={t("activeshells.title", { name })}>
      <p className="min-w-0 text-sm text-muted-foreground break-words">
        {t("activeshells.caption")}
      </p>
      {shells.isPending ? <TableSkeleton swarm={swarm} /> : null}
      {shells.isError && !shells.data ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>
              {t("activeshells.loadFailed")} {t("activeshells.loadFailedBody")}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={shells.isFetching}
              onClick={() => void shells.refetch()}
            >
              {shells.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("activeshells.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {shells.data && items.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Terminal aria-hidden="true" />
            </EmptyMedia>
            <EmptyTitle>{t("activeshells.empty.heading")}</EmptyTitle>
            <EmptyDescription>{t("activeshells.empty.body", { name })}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : null}
      {items.length > 0 ? (
        <div className="max-h-96 overflow-x-auto overflow-y-auto rounded-md ring-1 ring-foreground/10">
          <Table aria-label={t("activeshells.label", { name })}>
            <TableHeader className="sticky top-0 z-10 bg-muted">
              <TableRow>
                <TableHead>{t("activeshells.columns.user")}</TableHead>
                <TableHead>{t("activeshells.columns.container")}</TableHead>
                {swarm ? <TableHead>{t("activeshells.columns.node")}</TableHead> : null}
                <TableHead>{t("activeshells.columns.started")}</TableHead>
                <TableHead>{t("activeshells.columns.idle")}</TableHead>
                <TableHead className="w-12">
                  <span className="sr-only">{t("activeshells.columns.actions")}</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((s) => (
                <TableRow key={s.id}>
                  <TableCell>
                    <TruncatedText>{s.user}</TruncatedText>
                  </TableCell>
                  <TableCell>
                    <TruncatedText mono>{s.container}</TruncatedText>
                  </TableCell>
                  {swarm ? (
                    <TableCell>
                      <TruncatedText className="max-w-48">{s.node}</TruncatedText>
                    </TableCell>
                  ) : null}
                  <TableCell>
                    <RelativeTime value={s.startedAt} />
                  </TableCell>
                  <TableCell>
                    <IdleCell seconds={s.idleSeconds} />
                  </TableCell>
                  <TableCell>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <Button
                          type="button"
                          variant="ghost"
                          size="icon-sm"
                          className="max-md:size-11"
                          aria-label={t("activeshells.actionsFor", { user: s.user })}
                        >
                          <EllipsisVertical aria-hidden="true" />
                        </Button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end">
                        <DropdownMenuItem variant="destructive" onSelect={() => setEnding(s)}>
                          {t("activeshells.end.action")}
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      ) : null}
      <EndShellDialog envId={envId} shell={ending} onClose={() => setEnding(null)} />
    </SettingsCard>
  );
}
