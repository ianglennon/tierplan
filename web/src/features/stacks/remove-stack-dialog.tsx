// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useId, useRef, useState } from "react";

import {
  type DeployRequestError,
  invalidateStacks,
  type RemovalPreview,
  removalPreviewQuery,
  type StackDetail,
  useRemoveStack,
} from "@/api/stacks";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Spinner } from "@/components/ui/spinner";
import { followStackOperation } from "@/features/stacks/follow-operation";
import { RemovalProgress } from "@/features/stacks/removal-progress";
import { VolumePicker, volumeKey } from "@/features/stacks/volume-picker";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

interface RemoveStackDialogProps {
  stack: Pick<StackDetail, "id" | "envId" | "name" | "target">;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// refusalKey is the key of the words for a removal the server refused or could not reach.
function refusalKey(error: DeployRequestError): string {
  if (error.status === 0) return "stackRemove.refusal.unreachable";
  switch (error.type) {
    case "operation_locked":
    case "confirm_mismatch":
    case "volume_not_owned":
    case "stack_not_found":
      return `stackRemove.refusal.${error.type}`;
    default:
      return "stackRemove.refusal.other";
  }
}

interface Row {
  kind: "service" | "container" | "network" | "secret" | "config" | "definition";
  name: string;
}

// rowsOf are the lines of "What gets removed": what the stack owns, kind by kind, then its definition. The list
// always has the definition, so it is never empty.
function rowsOf(preview: RemovalPreview, stackName: string): Row[] {
  const rows: Row[] = [];
  for (const [kind, names] of [
    ["service", preview.services],
    ["container", preview.containers],
    ["network", preview.networks],
    ["secret", preview.secrets],
    ["config", preview.configs],
  ] as const) {
    for (const name of names ?? []) rows.push({ kind, name });
  }
  rows.push({ kind: "definition", name: stackName });
  return rows;
}

// RemoveStackDialog is "Remove {name}?" and, once the server has taken the removal, the panel that follows it:
// one dialog, two phases, as the pull dialog (UI-SPEC "Remove dialog and removal job"). The first phase lists
// what the stack owns and asks for its name typed; the confirm is the one destructive fill and stays
// aria-disabled until the list has loaded and the name matches. A refusal stays in the dialog as an alert and
// never raises a toast over it. Closing the second phase never stops the job: it runs on the server, and the
// answer comes as a toast from followStackOperation, which belongs to no component. Names are text.
export function RemoveStackDialog({ stack, open, onOpenChange }: RemoveStackDialogProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const remove = useRemoveStack(stack.envId, stack.id);
  const [view, setView] = useState<"form" | "progress">("form");
  const [jobId, setJobId] = useState<string | null>(null);
  const [typed, setTyped] = useState("");
  // The volumes that are ticked (by volumeKey) and whether the definition stays: both start unticked, and the
  // person's own hands tick them.
  const [ticked, setTicked] = useState<string[]>([]);
  const [keep, setKeep] = useState(false);
  // What the removal that started was asked to do, for the panel that follows it.
  const [keptDefinition, setKeptDefinition] = useState(false);
  const keepId = useId();
  const [refusal, setRefusal] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const preview = useQuery({
    ...removalPreviewQuery(stack.envId, stack.id),
    enabled: open && view === "form",
  });

  // The panel replacing the form takes focus on its heading.
  useEffect(() => {
    if (view === "progress" && open) headingRef.current?.focus();
  }, [view, open]);

  function change(next: boolean) {
    if (!next && view === "form") {
      setTyped("");
      setTicked([]);
      setKeep(false);
      setRefusal(null);
    }
    onOpenChange(next);
  }

  const volumes = preview.data?.volumes ?? [];
  const armed = Boolean(preview.data) && matchesName(typed, stack.name);
  const pending = submitting || remove.isPending;

  async function confirm() {
    if (!armed || pending) return;
    setSubmitting(true);
    setRefusal(null);
    try {
      const chosen = volumes.filter((v) => ticked.includes(volumeKey(v)));
      const started = await remove.mutateAsync({
        confirmName: stack.name,
        volumes: chosen.map((v) => ({ name: v.name, nodeId: v.nodeId })),
        keepDefinition: keep,
      });
      setKeptDefinition(keep);
      setJobId(started.jobId);
      setView("progress");
      followStackOperation(queryClient, {
        envId: stack.envId,
        stackId: stack.id,
        jobId: started.jobId,
        onDone: () => {
          invalidateStacks(queryClient, stack.envId);
          toastSuccess(t("stackRemove.toast.removed", { name: stack.name }));
        },
        onFailed: (reason) => {
          invalidateStacks(queryClient, stack.envId);
          toastError(
            reason
              ? t("stackRemove.toast.failed", { name: stack.name, reason })
              : t("stackRemove.toast.failedNoReason", { name: stack.name }),
          );
        },
      });
    } catch (e) {
      setRefusal(
        t("stackRemove.dialog.failed", {
          name: stack.name,
          reason: t(refusalKey(e as DeployRequestError)),
        }),
      );
    } finally {
      setSubmitting(false);
    }
  }

  const rows = preview.data ? rowsOf(preview.data, stack.name) : [];
  const counts = preview.data
    ? t("stackRemove.list.counts", {
        services:
          stack.target === "standalone"
            ? t("stackRemove.list.containers", { count: preview.data.containers?.length ?? 0 })
            : t("stackRemove.list.services", { count: preview.data.services?.length ?? 0 }),
        networks: t("stackRemove.list.networks", { count: preview.data.networks?.length ?? 0 }),
        secrets: t("stackRemove.list.secrets", { count: preview.data.secrets?.length ?? 0 }),
        configs: t("stackRemove.list.configs", { count: preview.data.configs?.length ?? 0 }),
      })
    : "";

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-160">
        {view === "progress" && jobId ? (
          <RemovalProgress
            envId={stack.envId}
            stackId={stack.id}
            stackName={stack.name}
            jobId={jobId}
            keepDefinition={keptDefinition}
            headingRef={headingRef}
            onClose={() => onOpenChange(false)}
          />
        ) : (
          <>
            <DialogHeader>
              <DialogTitle className="break-all">
                {t("stackRemove.dialog.title", { name: stack.name })}
              </DialogTitle>
              <DialogDescription>
                {t(
                  stack.target === "standalone"
                    ? "stackRemove.dialog.bodyContainers"
                    : "stackRemove.dialog.bodyServices",
                )}
              </DialogDescription>
            </DialogHeader>
            {preview.isPending && preview.fetchStatus !== "idle" ? (
              <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
                <Spinner aria-hidden="true" />
                <span className="break-all">
                  {t("stackRemove.dialog.loading", { name: stack.name })}
                </span>
              </div>
            ) : null}
            {preview.isError ? (
              <Alert variant="destructive" role="alert">
                <AlertDescription className="flex flex-wrap items-center gap-3">
                  <span className="break-words">
                    {t("stackRemove.dialog.loadFailed", { name: stack.name })}
                  </span>
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="max-md:h-11"
                    disabled={preview.isFetching}
                    onClick={() => void preview.refetch()}
                  >
                    {t("stackRemove.dialog.retry")}
                  </Button>
                </AlertDescription>
              </Alert>
            ) : null}
            {preview.data ? (
              <section className="flex flex-col gap-2">
                <h3 className="text-sm font-semibold">{t("stackRemove.list.heading")}</h3>
                <p className="text-sm text-muted-foreground">{counts}</p>
                <div
                  data-slot="target-list"
                  className="max-h-60 overflow-y-auto rounded-lg ring-1 ring-foreground/10"
                >
                  <table className="w-full text-sm">
                    <caption className="sr-only">
                      {t("stackRemove.list.caption", { name: stack.name })}
                    </caption>
                    <thead className="sticky top-0 bg-muted text-xs text-muted-foreground">
                      <tr>
                        <th scope="col" className="px-2 py-1 text-start font-semibold">
                          {t("stackRemove.list.kind")}
                        </th>
                        <th scope="col" className="px-2 py-1 text-start font-semibold">
                          {t("stackRemove.list.name")}
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {rows.map((r) => (
                        <tr key={`${r.kind}/${r.name}`} className="border-t">
                          <td className="px-2 py-1 text-xs">
                            {t(`stackRemove.list.kinds.${r.kind}`)}
                          </td>
                          <td className="px-2 py-1 font-mono text-xs break-all">{r.name}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              </section>
            ) : null}
            {preview.data ? (
              <>
                {volumes.length > 0 ? (
                  <VolumePicker
                    volumes={volumes}
                    value={ticked}
                    onChange={setTicked}
                    swarm={stack.target !== "standalone"}
                    disabled={pending}
                  />
                ) : (
                  <p className="text-sm text-muted-foreground">{t("stackRemove.volumes.empty")}</p>
                )}
                {(preview.data.notReporting ?? []).length > 0 ? (
                  <p className="text-sm text-muted-foreground break-words">
                    {t("stackRemove.volumes.notReporting", {
                      count: (preview.data.notReporting ?? []).length,
                      nodes: (preview.data.notReporting ?? []).join(", "),
                    })}
                  </p>
                ) : null}
                <div className="flex items-start gap-2">
                  <Checkbox
                    id={keepId}
                    checked={keep}
                    disabled={pending}
                    onCheckedChange={(next) => setKeep(next === true)}
                    className="mt-0.5 max-md:size-6"
                  />
                  <div className="flex flex-col gap-0.5">
                    <Label htmlFor={keepId}>{t("stackRemove.keep.label")}</Label>
                    <p className="text-sm text-muted-foreground">{t("stackRemove.keep.helper")}</p>
                  </div>
                </div>
                {preview.data.hasDraft && !keep ? (
                  <p className="text-sm">{t("stackRemove.draft")}</p>
                ) : null}
              </>
            ) : null}
            <TypeToConfirm
              name={stack.name}
              value={typed}
              onChange={setTyped}
              onConfirm={() => void confirm()}
              disabled={pending}
            />
            {refusal ? (
              <Alert variant="destructive" role="alert">
                <AlertDescription className="break-words">{refusal}</AlertDescription>
              </Alert>
            ) : null}
            <DialogFooter>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11"
                autoFocus
                onClick={() => change(false)}
              >
                {t("stackRemove.dialog.cancel")}
              </Button>
              <Button
                type="button"
                variant="destructive"
                className="max-md:h-11"
                aria-disabled={!armed || pending}
                onClick={() => void confirm()}
              >
                {pending ? <Spinner aria-hidden="true" /> : null}
                {ticked.length > 0
                  ? t("stackRemove.dialog.confirmVolumes", { count: ticked.length })
                  : t("stackRemove.dialog.confirm")}
              </Button>
            </DialogFooter>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
