// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";
import { useRef, useState } from "react";
import { Trans } from "react-i18next";

import { IngressActionError } from "@/api/ingress";
import {
  type IngressNode,
  type RevokeIngressNodeResult,
  useRevokeIngressNode,
} from "@/api/ingress-nodes";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
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
import { Checkbox } from "@/components/ui/checkbox";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The longest reason the server takes.
const REASON_MAX = 256;

interface RevokeNodeDialogProps {
  envId: string;
  // The node being revoked. Null while the dialog is closed.
  node: IngressNode | null;
  onClose: () => void;
  // Where focus goes when the dialog has closed. The row's button is the natural place, but the row can be gone.
  onCloseAutoFocus?: (event: Event) => void;
}

// RevokeNodeDialog is "Revoke {{name}}?" (D-13, UI-SPEC S4): an alert-dialog with focus on "Keep node" and a destructive
// confirm that says what revoking does. A refusal stays in the dialog as the server's own words. After success the body
// is replaced in place (no second dialog) by what to do next: rotate the join token, and request new certificates for
// the hostnames whose keys the node was sent. Radix closes an alert-dialog on any action click, so the confirm is a plain
// button and the dialog closes only when the person closes it.
export function RevokeNodeDialog({
  envId,
  node,
  onClose,
  onCloseAutoFocus,
}: RevokeNodeDialogProps) {
  const t = useT();
  // The dialog keeps the node it was opened for until it has closed, so the title does not lose the name while it
  // fades out.
  const shown = useRef<IngressNode | null>(null);
  if (node) shown.current = node;
  const name = shown.current?.name ?? "";
  const revoke = useRevokeIngressNode(envId);
  const [done, setDone] = useState<RevokeIngressNodeResult | null>(null);
  const [reason, setReason] = useState("");
  const [allowReenrol, setAllowReenrol] = useState(false);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  function close(open: boolean) {
    if (open || revoke.isPending) return;
    setDone(null);
    setReason("");
    setAllowReenrol(false);
    revoke.reset();
    onClose();
  }

  function submit() {
    if (!node || inFlight.current) return;
    inFlight.current = true;
    revoke.mutate(
      { nodeId: node.id, reason, allowReenrol },
      {
        onSuccess: (answer) => setDone(answer),
        onSettled: () => {
          inFlight.current = false;
        },
      },
    );
  }

  const detail = revoke.error instanceof IngressActionError ? revoke.error.detail : undefined;
  const held = done?.heldHostnames ?? [];

  return (
    <AlertDialog open={node !== null} onOpenChange={close}>
      <AlertDialogContent onCloseAutoFocus={onCloseAutoFocus}>
        {done ? (
          <>
            <Alert>
              <AlertDialogTitle asChild>
                <AlertTitle>{t("ingressPage.nodes.revoke.doneTitle", { name })}</AlertTitle>
              </AlertDialogTitle>
              <AlertDialogDescription asChild>
                <AlertDescription>{t("ingressPage.nodes.revoke.doneBody")}</AlertDescription>
              </AlertDialogDescription>
            </Alert>
            <div>
              <Button asChild type="button" variant="outline" className={TOUCH_HEIGHT}>
                <Link to="/env/$envId/ingress/settings" params={{ envId }} hash="join-token">
                  {t("ingressPage.nodes.revoke.joinToken")}
                </Link>
              </Button>
            </div>
            {held.length > 0 ? (
              <p className="text-xs break-words">
                <Trans
                  i18nKey="ingressPage.nodes.revoke.held"
                  ns="ingressPage"
                  components={{
                    names: (
                      <>
                        {held.map((h, i) => (
                          <span key={h}>
                            {i > 0 ? ", " : ""}
                            <span className="font-mono break-all">{h}</span>
                          </span>
                        ))}
                      </>
                    ),
                  }}
                />
              </p>
            ) : null}
            <AlertDialogFooter>
              <AlertDialogCancel className={TOUCH_HEIGHT}>
                {t("ingressPage.nodes.revoke.close")}
              </AlertDialogCancel>
            </AlertDialogFooter>
          </>
        ) : (
          <>
            <AlertDialogHeader>
              <AlertDialogTitle>{t("ingressPage.nodes.revoke.title", { name })}</AlertDialogTitle>
              <AlertDialogDescription>{t("ingressPage.nodes.revoke.body")}</AlertDialogDescription>
            </AlertDialogHeader>
            {revoke.isError ? (
              <Alert variant="destructive" role="alert">
                <AlertDescription className="break-words">
                  {detail ?? t("common.mutationFailed")}
                </AlertDescription>
              </Alert>
            ) : null}
            <FieldGroup className="gap-4">
              <Field>
                <FieldLabel htmlFor="revoke-node-reason">
                  {t("ingressPage.nodes.revoke.reason")}
                </FieldLabel>
                <Input
                  id="revoke-node-reason"
                  value={reason}
                  maxLength={REASON_MAX}
                  onChange={(e) => setReason(e.target.value)}
                  autoComplete="off"
                  disabled={revoke.isPending}
                  className={TOUCH_HEIGHT}
                  aria-describedby="revoke-node-reason-helper"
                />
                <FieldDescription id="revoke-node-reason-helper">
                  {t("ingressPage.nodes.revoke.reasonHelper")}
                </FieldDescription>
              </Field>
              <Field orientation="horizontal">
                <Checkbox
                  id="revoke-node-reenrol"
                  checked={allowReenrol}
                  disabled={revoke.isPending}
                  onCheckedChange={(next) => setAllowReenrol(next === true)}
                />
                <FieldLabel htmlFor="revoke-node-reenrol" className="font-normal">
                  {t("ingressPage.nodes.revoke.allowReenrol")}
                </FieldLabel>
              </Field>
            </FieldGroup>
            <AlertDialogFooter>
              {/* Radix focuses the dismiss button when the dialog opens. */}
              <AlertDialogCancel disabled={revoke.isPending} className={TOUCH_HEIGHT}>
                {t("ingressPage.nodes.revoke.keep")}
              </AlertDialogCancel>
              <Button
                type="button"
                variant="destructive"
                disabled={revoke.isPending}
                className={`${TOUCH_HEIGHT} bg-destructive-fill text-white hover:bg-destructive-fill/90`}
                onClick={submit}
              >
                {revoke.isPending ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("ingressPage.nodes.revoke.pending")}
                  </>
                ) : (
                  t("ingressPage.nodes.revoke.confirm")
                )}
              </Button>
            </AlertDialogFooter>
          </>
        )}
      </AlertDialogContent>
    </AlertDialog>
  );
}
