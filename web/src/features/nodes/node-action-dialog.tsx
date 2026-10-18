// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import {
  isLostNodeAction,
  isNodeGone,
  NodeActionError,
  type NodeImpact,
  type NodeImpactAction,
  nodeImpactQuery,
  useSetNodeAvailability,
  useSetNodeRole,
} from "@/api/nodes";
import { matchesName, TypeToConfirm } from "@/components/type-to-confirm";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { TargetList, type TargetRow } from "@/features/resources/target-list";
import { useEnvName } from "@/features/resources/use-env-name";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Translate = ReturnType<typeof useT>;

export interface NodeActionDialogProps {
  envId: string;
  // The node the change is for, or null while the dialog is closed.
  node: { id: string; hostname: string } | null;
  mode: NodeImpactAction;
  onClose: () => void;
  // Called once the swarm has accepted the change, after the dialog asked to close.
  onDone?: (change: { nodeId: string; action: NodeImpactAction }) => void;
}

interface WordingContext {
  // The node's name, for the words about it.
  name: string;
  // The display name of the environment, for the words about a swarm with no manager.
  environment: string;
}

// nodeActionWording words a change to a node that did not happen, from the problem type the
// server named. A type is never shown; Docker's own words are, where the server passes them on,
// because they say what it refused. A request that got no answer may well have gone through.
export function nodeActionWording(t: Translate, err: unknown, ctx: WordingContext): string {
  const e = err instanceof NodeActionError ? err : new NodeActionError(0);
  if (isLostNodeAction(e)) return t("actions.refusal.lost");
  switch (e.type) {
    case "last_manager":
    case "node_changed":
      return t(`nodes.action.refusal.${e.type}`);
    case "node_not_found":
    case "confirmation_required":
      return t(`nodes.action.refusal.${e.type}`, { name: ctx.name });
    case "no_manager_connected":
      return t("services.refusal.no_manager_connected", { environment: ctx.environment });
    case "no_quorum":
      return t("services.refusal.no_quorum");
    case "host_read_only":
      return t("actions.refusal.host_read_only");
    case "agent_policy_denied":
      return t("actions.refusal.agent_policy_denied");
    case "node_update_failed":
      if (e.detail) return t("nodes.action.refusal.rejected", { message: e.detail });
      return t("actions.refusal.generic");
  }
  return e.status === 403 ? t("actions.refusal.permission") : t("actions.refusal.generic");
}

// taskName is what a task is called: the service and its slot, web.2, or for a global service's
// task the service alone.
function taskName(service: string, slot: number): string {
  return slot > 0 ? `${service}.${slot}` : service;
}

// NodeActionDialog is the dialog of Drain, Pause, Promote and Demote (UI-SPEC "Nodes"). It asks
// the server what the change will do and shows it: for a drain, where each task goes; for a
// demote, the managers that would be left. A spinner row stands in until the answer arrives and
// the confirm stays off, so a warning is never skipped. When the server says the node's name is
// needed (quorum at risk, the node that runs Bayplan, the only manager agent) the dialog asks for
// it and its confirm turns destructive and carries the name; the server asks for it too, so a
// stale page cannot skip it. A refusal stays inside the dialog in words, and the change is sent
// once, with the version the answer carried, and never retried.
export function NodeActionDialog({ envId, node, mode, onClose, onDone }: NodeActionDialogProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const query = useQuery({
    ...nodeImpactQuery(envId, node?.id ?? "", mode),
    enabled: node !== null,
  });
  const availability = useSetNodeAvailability(envId);
  const role = useSetNodeRole(envId);
  const sending = availability.isPending || role.isPending;
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const cancel = useRef<HTMLButtonElement>(null);
  const name = node?.hostname ?? "";
  const impact = query.data;
  const named = impact?.needsConfirm === true;
  const ready = impact !== undefined && (!named || matchesName(typed, name));
  const id = node?.id;

  // A dialog opened again, for another node or another change, starts clean.
  useEffect(() => {
    void id;
    void mode;
    setTyped("");
    setError(null);
  }, [id, mode]);

  async function submit() {
    if (!node || !impact || !ready || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    const confirm = named ? typed.trim() : undefined;
    try {
      if (mode === "drain" || mode === "pause") {
        await availability.mutateAsync({
          nodeId: node.id,
          availability: mode,
          version: impact.version,
          confirm,
        });
      } else {
        await role.mutateAsync({
          nodeId: node.id,
          role: mode === "promote" ? "manager" : "worker",
          version: impact.version,
          confirm,
        });
      }
      toastSuccess(t(`nodes.action.${mode}.done`, { name }));
      onClose();
      onDone?.({ nodeId: node.id, action: mode });
    } catch (e) {
      setError(nodeActionWording(t, e, { name, environment }));
    } finally {
      inFlight.current = false;
    }
  }

  function change(open: boolean) {
    if (open || sending) return;
    setError(null);
    onClose();
  }

  return (
    <Dialog open={node !== null} onOpenChange={change}>
      <DialogContent
        className="sm:max-w-140"
        onOpenAutoFocus={(event) => {
          // Focus starts on Cancel, the safe choice, not on the first field.
          event.preventDefault();
          cancel.current?.focus();
        }}
      >
        <DialogHeader>
          {/* A long name wraps anywhere rather than running off the dialog. */}
          <DialogTitle className="break-all">
            {t(`nodes.action.${mode}.title`, { name })}
          </DialogTitle>
          <DialogDescription>{describe(t, mode, impact)}</DialogDescription>
        </DialogHeader>

        {query.isPending ? (
          <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
            <Spinner aria-hidden="true" />
            <span>{t("nodes.action.checking")}</span>
          </div>
        ) : null}

        {query.isError ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription className="flex flex-wrap items-center gap-3">
              <span>
                {isNodeGone(query.error)
                  ? t("nodes.action.gone", { name })
                  : t("nodes.action.loadFailed")}
              </span>
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                disabled={query.isFetching}
                onClick={() => void query.refetch()}
              >
                {t("nodes.action.tryAgain")}
              </Button>
            </AlertDescription>
          </Alert>
        ) : null}

        {impact ? <Impact t={t} mode={mode} name={name} impact={impact} /> : null}

        {named ? (
          <TypeToConfirm
            name={name}
            value={typed}
            onChange={setTyped}
            onConfirm={() => void submit()}
            disabled={sending}
          />
        ) : null}

        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}

        <DialogFooter>
          <Button
            ref={cancel}
            type="button"
            variant="outline"
            disabled={sending}
            className={TOUCH_HEIGHT}
            onClick={() => change(false)}
          >
            {t("nodes.action.cancel")}
          </Button>
          <Button
            type="button"
            aria-disabled={sending || !ready}
            className={cn(
              TOUCH_HEIGHT,
              "aria-disabled:cursor-not-allowed aria-disabled:opacity-50",
              named && "bg-destructive-fill text-white hover:bg-destructive-fill/90",
            )}
            onClick={() => void submit()}
          >
            {sending ? (
              <>
                <Spinner aria-hidden="true" />
                {t(`nodes.action.${mode}.pending`)}
              </>
            ) : (
              // Only a drain or a demote can ask for the name, and then it is in the button.
              t(
                named && (mode === "drain" || mode === "demote")
                  ? `nodes.action.${mode}.confirmNamed`
                  : `nodes.action.${mode}.confirm`,
                { name },
              )
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

// describe is the sentence under the title: what the change does, and once the answer is in, how
// many tasks keep running or how many managers there will be.
function describe(t: Translate, mode: NodeImpactAction, impact: NodeImpact | undefined): string {
  const base = t(`nodes.action.${mode}.body`);
  if (!impact) return base;
  let extra = "";
  if (mode === "pause") {
    const count = impact.tasks?.length ?? 0;
    extra = count > 0 ? t("nodes.action.pause.tasks", { count }) : t("nodes.action.noTasks");
  } else if (mode === "promote" || mode === "demote") {
    extra = t(`nodes.action.${mode}.managers`, { count: impact.managers.after });
  }
  return extra ? `${base} ${extra}` : base;
}

interface ImpactProps {
  t: Translate;
  mode: NodeImpactAction;
  name: string;
  impact: NodeImpact;
}

// Impact is the warnings that apply and, for a drain, the tasks and what becomes of each.
function Impact({ t, mode, name, impact }: ImpactProps) {
  const doing = t(mode === "drain" ? "nodes.action.doing.drain" : "nodes.action.doing.demote");
  const { after, reachable, total } = impact.managers;
  const warning = (code: string): string | null => {
    switch (code) {
      case "even_count":
        return t("nodes.action.warning.even_count", {
          n: after,
          losses: Math.floor((after - 1) / 2),
          fewer: after - 1,
        });
      case "quorum_at_risk":
        return t("nodes.action.warning.quorum_at_risk", { reachable, total });
      case "runs_bayplan":
      case "only_manager_agent":
        return t(`nodes.action.warning.${code}`, { name, doing });
      case "facts_unreadable":
        return t("nodes.action.warning.facts_unreadable");
    }
    return null;
  };
  const warnings = (impact.warnings ?? []).map(warning).filter((w): w is string => w !== null);
  const tasks = impact.tasks ?? [];
  const rows: TargetRow[] = tasks.map((task) => ({
    name: taskName(task.service, task.slot),
    node: task.service,
    note: t(`nodes.action.outcome.${task.outcome}`),
  }));
  return (
    <>
      {warnings.map((text) => (
        <Alert key={text} variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="text-foreground">{text}</AlertDescription>
        </Alert>
      ))}
      {mode === "drain" ? (
        rows.length > 0 ? (
          <TargetList
            rows={rows}
            swarm
            label={t("nodes.action.tasks.task")}
            nodeLabel={t("nodes.action.tasks.service")}
            noteLabel={t("nodes.action.tasks.outcome")}
          />
        ) : (
          <p className="text-sm text-muted-foreground">{t("nodes.action.noTasks")}</p>
        )
      ) : null}
    </>
  );
}
