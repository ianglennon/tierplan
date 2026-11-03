// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type FormEvent, useId, useState } from "react";

import {
  JanitorRequestError,
  type TaskHistory,
  useSaveTaskHistory,
  useTaskHistory,
} from "@/api/janitor";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { parseWhole } from "@/features/janitor/policy-diff";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// The range the swarm accepts (a whole number from 0 to 1000). A swarm that keeps every task reports
// -1, which is outside it: TierPlan shows that and cannot save it.
export const TASK_HISTORY_MIN = 0;
export const TASK_HISTORY_MAX = 1000;

interface TaskHistoryLimitCardProps {
  envId: string;
  envName: string;
}

// What a refused read or save is worded as. no_manager is a swarm with no manager connected, which is
// a state and not a failure; the others are a sentence under the field.
type Refusal = "no_manager" | "swarm_changed" | "agent_policy_denied" | "host_read_only" | "other";

function refusalOf(error: unknown): Refusal {
  if (!(error instanceof JanitorRequestError)) return "other";
  switch (error.type) {
    case "no_manager":
    case "swarm_changed":
    case "agent_policy_denied":
    case "host_read_only":
      return error.type;
    default:
      return "other";
  }
}

// TaskHistoryLimitCard is the swarm's task history limit (anchor #task-history, D-17): how many old
// task records Docker keeps for each service slot, read live from a manager and changed here. TierPlan
// never changes it on its own. It is drawn on a swarm only. Changing it needs Env Admin
// (janitor.run); below that the field is visible and Save says what it needs. With no manager
// connected the limit is "Not measured" and neither read nor changed, while the policy and the run
// history still work (D-21).
export function TaskHistoryLimitCard({ envId, envName }: TaskHistoryLimitCardProps) {
  const t = useT();
  const id = useId();
  const history = useTaskHistory(envId);
  const save = useSaveTaskHistory(envId);
  const needs = useNeeds("janitor.run", envId);
  // The text of the field once the person has typed in it; null shows the swarm's own number.
  const [text, setText] = useState<string | null>(null);
  const [failure, setFailure] = useState<Refusal | null>(null);

  const readRefusal = history.isError ? refusalOf(history.error) : null;
  const notMeasured = readRefusal === "no_manager" || readRefusal === "agent_policy_denied";
  const live: TaskHistory | undefined = history.data;
  const keepsAll = live?.keepsAll === true;

  const canEdit = needs.allowed;
  const permissionReason = canEdit
    ? undefined
    : t("janitor.needsOn", { need: needs.reason ?? t("needs.role.admin"), name: envName });
  const noManagerReason = t("janitorNodes.taskHistory.noManager");

  const shown = text ?? (live ? String(live.limit) : "");
  const valid = parseWhole(shown, TASK_HISTORY_MIN, TASK_HISTORY_MAX);
  const changed = text !== null && live !== undefined && !keepsAll && valid !== live.limit;
  const showError = text !== null && valid === null;

  function edit(next: string) {
    if (!canEdit || !live || keepsAll || save.isPending) return;
    // Typing the swarm's own number back is no change at all.
    setText(next.trim() === String(live.limit) ? null : next);
    setFailure(null);
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!canEdit || save.isPending || !changed || valid === null) return;
    try {
      const done = await save.mutateAsync(valid);
      setText(null);
      setFailure(null);
      toastSuccess(t("janitorNodes.taskHistory.saved", { count: done.limit }));
    } catch (e) {
      setFailure(refusalOf(e));
    }
  }

  // The reason the button cannot be pressed, in the order a person can act on it: the role first, then
  // the swarm having no manager.
  const buttonReason = permissionReason ?? (notMeasured ? noManagerReason : undefined);
  const button = (
    <Button
      type="submit"
      variant="outline"
      className="max-md:h-11 max-md:w-full"
      aria-disabled={save.isPending || !changed || valid === null || undefined}
    >
      {save.isPending ? (
        <>
          <Spinner aria-hidden="true" />
          {t("janitorNodes.taskHistory.saving")}
        </>
      ) : (
        t("janitorNodes.taskHistory.save")
      )}
    </Button>
  );

  const failureText =
    failure === null
      ? null
      : failure === "swarm_changed"
        ? t("janitorNodes.taskHistory.swarmChanged")
        : failure === "agent_policy_denied"
          ? t("janitorNodes.taskHistory.agentPolicyDenied")
          : failure === "host_read_only"
            ? t("janitorNodes.taskHistory.hostReadOnly")
            : failure === "no_manager"
              ? noManagerReason
              : `${t("janitorNodes.taskHistory.saveFailed")} ${t("janitorNodes.taskHistory.retry")}`;

  return (
    <section
      id="task-history"
      aria-label={t("janitorNodes.taskHistory.title")}
      className="scroll-mt-4"
    >
      <Card>
        <CardHeader>
          <CardTitle>{t("janitorNodes.taskHistory.title")}</CardTitle>
          <CardDescription>{t("janitorNodes.taskHistory.description")}</CardDescription>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {history.isPending ? (
            <div
              role="status"
              aria-label={t("janitorNodes.taskHistory.loading")}
              aria-busy="true"
              className="flex flex-col gap-3"
            >
              <Skeleton className="h-8 w-40" aria-hidden="true" />
              <Skeleton className="h-8 w-56" aria-hidden="true" />
            </div>
          ) : history.isError && !notMeasured ? (
            <Alert variant="destructive">
              <AlertTitle>{t("janitorNodes.taskHistory.loadFailed")}</AlertTitle>
              <AlertDescription className="flex flex-wrap items-center gap-3">
                <Button
                  variant="outline"
                  size="sm"
                  disabled={history.isFetching}
                  onClick={() => void history.refetch()}
                >
                  {history.isFetching ? <Spinner aria-hidden="true" /> : null}
                  {t("janitorNodes.taskHistory.loadRetry")}
                </Button>
              </AlertDescription>
            </Alert>
          ) : (
            <form onSubmit={submit} noValidate className="flex flex-col gap-4">
              <div className="flex min-w-0 flex-col gap-2">
                <Label htmlFor={`${id}-limit`} className="text-sm font-medium">
                  {t("janitorNodes.taskHistory.field")}
                </Label>
                {notMeasured ? (
                  <>
                    <Input
                      id={`${id}-limit`}
                      type="text"
                      readOnly
                      aria-disabled="true"
                      value={t("janitorNodes.taskHistory.notMeasured")}
                      aria-describedby={`${id}-reason`}
                      className="w-56 max-md:h-11"
                    />
                    <p id={`${id}-reason`} className="text-sm text-muted-foreground">
                      {readRefusal === "agent_policy_denied"
                        ? t("janitorNodes.taskHistory.agentPolicyDenied")
                        : noManagerReason}
                    </p>
                  </>
                ) : keepsAll && live ? (
                  <Input
                    id={`${id}-limit`}
                    type="text"
                    readOnly
                    value={t("janitorNodes.taskHistory.keepsAll", { count: live.limit })}
                    aria-describedby={`${id}-helper`}
                    className="w-56 max-md:h-11"
                  />
                ) : (
                  <Input
                    id={`${id}-limit`}
                    type="number"
                    inputMode="numeric"
                    min={TASK_HISTORY_MIN}
                    max={TASK_HISTORY_MAX}
                    step={1}
                    value={shown}
                    readOnly={!canEdit}
                    aria-invalid={showError || undefined}
                    aria-describedby={`${id}-helper ${id}-error`}
                    className="w-28 max-md:h-11"
                    onChange={(e) => edit(e.target.value)}
                  />
                )}
                <p id={`${id}-helper`} className="text-sm text-muted-foreground">
                  {t("janitorNodes.taskHistory.helper", { default: live?.dockerDefault ?? 5 })}
                </p>
                {showError ? (
                  <p id={`${id}-error`} role="alert" className="text-sm text-destructive">
                    {t("janitorNodes.taskHistory.error")}
                  </p>
                ) : null}
                <p role="status" className="text-sm text-muted-foreground">
                  {live ? t("janitorNodes.taskHistory.current", { count: live.limit }) : ""}
                </p>
              </div>
              {failureText ? (
                <p role="alert" className="text-sm text-destructive">
                  {failureText}
                </p>
              ) : null}
              <div>
                <NeedsTooltip reason={buttonReason}>{button}</NeedsTooltip>
              </div>
            </form>
          )}
        </CardContent>
      </Card>
    </section>
  );
}
