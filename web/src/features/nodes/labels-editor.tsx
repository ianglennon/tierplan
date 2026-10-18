// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useMemo, useRef, useState } from "react";

import {
  NodeActionError,
  type NodeDetail,
  useNodeLabelImpact,
  useSetNodeLabels,
} from "@/api/nodes";
import { entriesOf, KeyValueEditor, type KeyValueRow, newRow } from "@/components/key-value-editor";
import { KeyValueTable } from "@/components/key-value-table";
import { NeedsTooltip } from "@/components/needs-tooltip";
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
import { LabelImpactPanel } from "@/features/nodes/label-impact-panel";
import { nodeActionWording } from "@/features/nodes/node-action-dialog";
import { checkLabelRows } from "@/features/nodes/reserved-labels";
import { useEnvName } from "@/features/resources/use-env-name";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// The check of the labels is asked this long after the last edit.
export const LABEL_CHECK_DEBOUNCE_MS = 400;

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "max-md:h-11";

type Labels = Record<string, string>;

const byName = ([a]: [string, string], [b]: [string, string]) => (a < b ? -1 : a > b ? 1 : 0);

function rowsOf(labels: Labels): KeyValueRow[] {
  return Object.entries(labels)
    .sort(byName)
    .map(([key, value]) => newRow(key, value));
}

function sameLabels(a: Labels, b: Labels): boolean {
  const ak = Object.keys(a);
  return ak.length === Object.keys(b).length && ak.every((k) => b[k] === a[k]);
}

export interface NodeLabelsEditorProps {
  envId: string;
  node: Pick<NodeDetail, "id" | "hostname" | "labels">;
}

// NodeLabelsEditor is the Labels tab of a node (UI-SPEC "Nodes", D-21, E11, E15): the node's
// labels as a table of names and values, and under it the Effect on services. Every edit is
// checked with the server 400 ms after the last one; Save labels stays off until the answer for the
// labels as they are now is in, so a change is never saved unseen. When the answer says a
// constraint would no longer match, saving asks first. A name Docker keeps for itself is refused
// here and, again, on the server. A person below Admin sees the table and an unavailable Save.
export function NodeLabelsEditor({ envId, node }: NodeLabelsEditorProps) {
  const t = useT();
  const environment = useEnvName(envId);
  const needs = useNeeds("nodes.manage", envId);
  const [baseline, setBaseline] = useState<Labels>(node.labels);
  const [rows, setRows] = useState<KeyValueRow[]>(() => rowsOf(node.labels));
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Bumped to ask the server again for the labels as they are.
  const [recheck, setRecheck] = useState(0);
  const inFlight = useRef(false);

  const labels = useMemo(() => entriesOf(rows), [rows]);
  const check = useMemo(() => checkLabelRows(rows), [rows]);
  const dirty = !sameLabels(labels, baseline);
  const canEdit = needs.allowed;
  const checkable = canEdit && dirty && check.valid;

  const impactMutation = useNodeLabelImpact(envId, node.id);
  const { mutate: askImpact, reset: resetImpact } = impactMutation;
  const save = useSetNodeLabels(envId);

  // The node changed in the swarm while nothing here was edited: show it as it is now.
  const nodeLabels = node.labels;
  useEffect(() => {
    if (sameLabels(entriesOf(rows), baseline) && !sameLabels(nodeLabels, baseline)) {
      setBaseline(nodeLabels);
      setRows(rowsOf(nodeLabels));
    }
  }, [nodeLabels, rows, baseline]);

  useEffect(() => {
    void recheck;
    if (!checkable) {
      resetImpact();
      return;
    }
    const timer = setTimeout(() => askImpact(labels), LABEL_CHECK_DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [labels, checkable, recheck, askImpact, resetImpact]);

  const asked = impactMutation.variables;
  const forThese = asked !== undefined && sameLabels(asked, labels);
  const impact =
    checkable && forThese && impactMutation.isSuccess ? impactMutation.data : undefined;
  const failed = checkable && forThese && impactMutation.isError;
  const status = !checkable ? "idle" : impact ? "ready" : failed ? "error" : "pending";

  function retry() {
    resetImpact();
    setRecheck((n) => n + 1);
  }

  async function doSave() {
    if (!impact || inFlight.current) return;
    inFlight.current = true;
    setError(null);
    try {
      await save.mutateAsync({ nodeId: node.id, labels, version: impact.version });
      toastSuccess(t("nodes.labels.saved", { name: node.hostname }));
      setBaseline(labels);
    } catch (e) {
      const err = e instanceof NodeActionError ? e : new NodeActionError(0);
      setError(wording(err));
      // A node that changed is judged again, so that the next save carries its version now.
      if (err.type === "node_changed") retry();
    } finally {
      inFlight.current = false;
    }
  }

  function wording(err: NodeActionError): string {
    if (err.type === "node_changed") return t("nodes.labels.refusal.node_changed");
    if ((err.type === "label_reserved" || err.type === "label_invalid") && err.detail) {
      return err.detail;
    }
    return nodeActionWording(t, err, { name: node.hostname, environment });
  }

  function onSave() {
    if (!impact || save.isPending) return;
    if (impact.noLongerMatches > 0) setConfirming(true);
    else void doSave();
  }

  function discard() {
    setRows(rowsOf(baseline));
    setError(null);
  }

  const problems = new Map<string, string>();
  for (const [id, problem] of check.rows) {
    problems.set(
      id,
      t(`nodes.labels.problem.${problem.kind}`, {
        prefix: problem.kind === "reserved" ? problem.prefix : undefined,
      }),
    );
  }

  const saveHeld = !impact || save.isPending;
  const saveButton = (
    <Button
      type="button"
      className={TOUCH_HEIGHT}
      disabled={needs.reason ? undefined : !canEdit || saveHeld}
      onClick={onSave}
    >
      {t("nodes.labels.save")}
    </Button>
  );

  return (
    <div className="flex flex-col gap-6">
      <section aria-labelledby="node-labels-heading" className="flex flex-col gap-3">
        <h3 id="node-labels-heading" className="text-base font-semibold">
          {t("nodes.labels.title")}
        </h3>
        <p className="text-sm text-muted-foreground">{t("nodes.labels.helper")}</p>

        {canEdit ? (
          <>
            {rows.length === 0 ? (
              <p className="text-sm text-muted-foreground">{t("nodes.labels.empty")}</p>
            ) : null}
            <KeyValueEditor
              value={rows}
              onChange={setRows}
              errors={problems}
              labels={{
                add: t("nodes.labels.add"),
                key: t("nodes.labels.key"),
                value: t("nodes.labels.value"),
                remove: (key) =>
                  key ? t("nodes.labels.remove", { name: key }) : t("nodes.labels.removeEmpty"),
              }}
            />
            {check.tooMany ? (
              <p className="text-xs text-destructive">{t("nodes.labels.problem.tooMany")}</p>
            ) : null}
          </>
        ) : (
          <KeyValueTable
            label={t("nodes.labels.title")}
            empty={t("nodes.labels.empty")}
            rows={Object.entries(node.labels)
              .sort(byName)
              .map(([key, value]) => ({ key, value }))}
          />
        )}

        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}

        <div className="flex flex-wrap items-center gap-2">
          {canEdit ? (
            <Button
              type="button"
              variant="ghost"
              className={TOUCH_HEIGHT}
              disabled={!dirty || save.isPending}
              onClick={discard}
            >
              {t("nodes.labels.discard")}
            </Button>
          ) : null}
          <NeedsTooltip reason={canEdit ? undefined : needs.reason}>{saveButton}</NeedsTooltip>
        </div>
      </section>

      <LabelImpactPanel status={status} impact={impact} onRetry={retry} />

      <AlertDialog open={confirming} onOpenChange={setConfirming}>
        <AlertDialogContent className="sm:max-w-160">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("nodes.labels.confirm.title")}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("nodes.labels.confirm.body", { count: impact?.noLongerMatches ?? 0 })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("nodes.labels.confirm.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => void doSave()}>
              {t("nodes.labels.confirm.apply")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
