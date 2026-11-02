// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useRef, useState } from "react";

import {
  type StackGitWebhookFlavour,
  type StackGitWebhookSecret,
  useCreateStackGitWebhook,
} from "@/api/stack-git";
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
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The git hosts in the order the UI-SPEC lists them.
export const WEBHOOK_FLAVOURS: readonly StackGitWebhookFlavour[] = [
  "github",
  "gitlab",
  "gitea",
  "generic",
];

type Placement = "path" | "header";

interface SetUpFormProps {
  envId: string;
  stackId: string;
  onCancel: () => void;
  onCreated: (answer: StackGitWebhookSecret) => void;
  onPendingChange: (pending: boolean) => void;
}

function SetUpForm({ envId, stackId, onCancel, onCreated, onPendingChange }: SetUpFormProps) {
  const t = useT();
  const create = useCreateStackGitWebhook(envId, stackId);
  const [flavour, setFlavour] = useState<StackGitWebhookFlavour>("github");
  const [placement, setPlacement] = useState<Placement>("path");
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);
  const pending = create.isPending;

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (inFlight.current) return;
    inFlight.current = true;
    onPendingChange(true);
    setError(null);
    try {
      const answer = await create.mutateAsync(
        flavour === "generic" ? { flavour, placement } : { flavour },
      );
      // The answer is in this call and nowhere else: the mutation forgets it before the caller keeps it in state.
      create.reset();
      toastSuccess(t("stackGit.webhook.created"));
      onCreated(answer);
    } catch (e) {
      const { status, type } = e as { status?: number; type?: string };
      if (type === "webhook_exists") setError(t("stackGit.webhook.setup.exists"));
      else if (status === 403) setError(t("stackGit.webhook.setup.permission"));
      else setError(t("stackGit.webhook.setup.failed"));
    } finally {
      inFlight.current = false;
      onPendingChange(false);
    }
  }

  return (
    <form noValidate onSubmit={(e) => void submit(e)} className="flex flex-col gap-4">
      {error ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      ) : null}
      <fieldset disabled={pending} className="flex min-w-0 flex-col gap-4">
        <div className="flex flex-col gap-2">
          <p id="webhook-host-label" className="text-sm font-medium">
            {t("stackGit.webhook.setup.host")}
          </p>
          <RadioGroup
            value={flavour}
            onValueChange={(v) => setFlavour(v as StackGitWebhookFlavour)}
            aria-labelledby="webhook-host-label"
            className="gap-1"
          >
            {WEBHOOK_FLAVOURS.map((f) => (
              <div key={f} className="flex min-h-11 items-center gap-2 md:min-h-8">
                <RadioGroupItem value={f} id={`webhook-host-${f}`} />
                <Label htmlFor={`webhook-host-${f}`}>{t(`stackGit.webhook.host.${f}`)}</Label>
              </div>
            ))}
          </RadioGroup>
        </div>
        {flavour === "generic" ? (
          <div className="flex flex-col gap-2">
            <p id="webhook-placement-label" className="text-sm font-medium">
              {t("stackGit.webhook.setup.placement")}
            </p>
            <RadioGroup
              value={placement}
              onValueChange={(v) => setPlacement(v as Placement)}
              aria-labelledby="webhook-placement-label"
              className="gap-1"
            >
              {(["path", "header"] as const).map((p) => (
                <div key={p} className="flex min-h-11 items-center gap-2 md:min-h-8">
                  <RadioGroupItem value={p} id={`webhook-placement-${p}`} />
                  <Label htmlFor={`webhook-placement-${p}`}>
                    {t(
                      p === "path"
                        ? "stackGit.webhook.setup.placementPath"
                        : "stackGit.webhook.setup.placementHeader",
                    )}
                  </Label>
                </div>
              ))}
            </RadioGroup>
          </div>
        ) : null}
      </fieldset>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={pending}
          onClick={onCancel}
        >
          {t("stackGit.webhook.setup.cancel")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} aria-disabled={pending}>
          {pending ? (
            <>
              <Spinner aria-hidden="true" />
              {t("stackGit.webhook.setup.creating")}
            </>
          ) : (
            t("stackGit.webhook.setup.create")
          )}
        </Button>
      </DialogFooter>
    </form>
  );
}

interface SetUpWebhookDialogProps {
  envId: string;
  stackId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Called with the address and secret the server made. The caller keeps them in its state, shows them once and drops
  // them; this dialog has closed by then.
  onCreated: (answer: StackGitWebhookSecret) => void;
}

// SetUpWebhookDialog is "Set up a webhook" (UI-SPEC "Git tab", D-15): the git host, and for the generic host where the
// secret goes. Create webhook makes the stack's one webhook; the dialog then closes and the card shows the show-once
// panel. The form starts again each time the dialog opens, and the dialog cannot be dismissed while the request is
// out.
export function SetUpWebhookDialog({
  envId,
  stackId,
  open,
  onOpenChange,
  onCreated,
}: SetUpWebhookDialogProps) {
  const t = useT();
  const [pending, setPending] = useState(false);

  function change(next: boolean) {
    if (!next && pending) return;
    onOpenChange(next);
  }

  return (
    <Dialog open={open} onOpenChange={change}>
      <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-140">
        <DialogHeader>
          <DialogTitle>{t("stackGit.webhook.setup.title")}</DialogTitle>
          <DialogDescription>{t("stackGit.webhook.setup.body")}</DialogDescription>
        </DialogHeader>
        <SetUpForm
          envId={envId}
          stackId={stackId}
          onCancel={() => change(false)}
          onCreated={(answer) => {
            onOpenChange(false);
            onCreated(answer);
          }}
          onPendingChange={setPending}
        />
      </DialogContent>
    </Dialog>
  );
}
