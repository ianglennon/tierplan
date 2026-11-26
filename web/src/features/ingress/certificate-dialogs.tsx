// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type FormEvent, useState } from "react";

import { IngressActionError } from "@/api/ingress";
import { useRequestCertificate } from "@/api/ingress-certs";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Refusal is a write the server refused, in the server's own words: the detail it sent, as sent, in a destructive
// alert that stays in the dialog. A failure that carries no detail (the network, a proxy) gets the common sentence.
function Refusal({ error }: { error: unknown }) {
  const t = useT();
  const detail = error instanceof IngressActionError ? error.detail : undefined;
  return (
    <Alert variant="destructive" role="alert">
      <AlertDescription className="break-words">
        {detail ?? t("common.mutationFailed")}
      </AlertDescription>
    </Alert>
  );
}

// RequestCertificateDialog asks for a certificate for a hostname ahead of any stack's claim (D-12, UI-SPEC S3). One
// field, so Enter submits. A refusal (a name outside the registered domains, one already requested) stays in the
// dialog; success closes it with a toast. The form is mounted only while the dialog is open, so it starts empty each
// time.
export function RequestCertificateDialog({
  envId,
  open,
  onOpenChange,
}: {
  envId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent aria-describedby={undefined}>
        <RequestForm envId={envId} onClose={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

function RequestForm({ envId, onClose }: { envId: string; onClose: () => void }) {
  const t = useT();
  const request = useRequestCertificate(envId);
  const [hostname, setHostname] = useState("");
  const host = hostname.trim();

  function submit(event: FormEvent) {
    event.preventDefault();
    if (host === "" || request.isPending) return;
    request.mutate(host, {
      onSuccess: () => {
        toastSuccess(t("ingressPage.certs.request.done", { host }));
        onClose();
      },
    });
  }

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle>{t("ingressPage.certs.request.title")}</DialogTitle>
      </DialogHeader>
      {request.isError ? <Refusal error={request.error} /> : null}
      <FieldGroup className="gap-4">
        <Field>
          <FieldLabel htmlFor="request-cert-hostname">
            {t("ingressPage.certs.request.hostname")}
          </FieldLabel>
          <Input
            id="request-cert-hostname"
            value={hostname}
            onChange={(e) => setHostname(e.target.value)}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            disabled={request.isPending}
            className={`${TOUCH_HEIGHT} font-mono text-xs`}
            aria-describedby="request-cert-hostname-helper"
          />
          <FieldDescription id="request-cert-hostname-helper">
            {t("ingressPage.certs.request.helper")}
          </FieldDescription>
        </Field>
      </FieldGroup>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={request.isPending}
          onClick={onClose}
        >
          {t("ingressPage.certs.request.discard")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={host === "" || request.isPending}>
          {request.isPending ? <Spinner aria-hidden="true" /> : null}
          {t("ingressPage.certs.request.submit")}
        </Button>
      </DialogFooter>
    </form>
  );
}
