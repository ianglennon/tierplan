// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type FormEvent, type RefObject, useState } from "react";

import { IngressActionError } from "@/api/ingress";
import {
  type IngressCert,
  useRequestCertificate,
  useSwitchCertificateToACME,
  useUploadCertificate,
} from "@/api/ingress-certs";
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
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldDescription, FieldGroup, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Textarea } from "@/components/ui/textarea";
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

// focusBack hands focus to the row's menu button when a dialog opened from that menu closes. The menu hands focus
// back to its button itself after the dialog has opened, and Radix has no trigger of the dialog's own to return to.
function focusBack(ref?: RefObject<HTMLElement | null>) {
  return (event: Event) => {
    if (!ref?.current) return;
    event.preventDefault();
    ref.current.focus();
  };
}

// UploadCertificateDialog takes a certificate chain and its private key for one hostname (D-12, UI-SPEC S3). The key
// is the one secret the page ever holds, so it lives only in this form's state: the form is mounted only while the
// dialog is open, so closing it, on success or on Discard upload, drops both fields. The key is never echoed after the
// request, never put in a toast and never in a live region. A refusal shows the server's detail and keeps the text, so
// it can be corrected (T-27-58).
export function UploadCertificateDialog({
  envId,
  host,
  open,
  onOpenChange,
  returnFocusRef,
}: {
  envId: string;
  host: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  returnFocusRef?: RefObject<HTMLElement | null>;
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent aria-describedby={undefined} onCloseAutoFocus={focusBack(returnFocusRef)}>
        <UploadForm envId={envId} host={host} onClose={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

function UploadForm({
  envId,
  host,
  onClose,
}: {
  envId: string;
  host: string;
  onClose: () => void;
}) {
  const t = useT();
  const upload = useUploadCertificate(envId);
  const [chain, setChain] = useState("");
  const [key, setKey] = useState("");
  const ready = chain.trim() !== "" && key.trim() !== "";

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!ready || upload.isPending) return;
    upload.mutate(
      { hostname: host, certificatePem: chain, privateKeyPem: key },
      {
        onSuccess: () => {
          toastSuccess(t("ingressPage.certs.upload.done", { host }));
          onClose();
        },
      },
    );
  }

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle className="break-words">
          {t("ingressPage.certs.upload.title", { host })}
        </DialogTitle>
      </DialogHeader>
      {upload.isError ? <Refusal error={upload.error} /> : null}
      <FieldGroup className="gap-4">
        <Field>
          <FieldLabel htmlFor="upload-cert-chain">{t("ingressPage.certs.upload.chain")}</FieldLabel>
          <Textarea
            id="upload-cert-chain"
            value={chain}
            onChange={(e) => setChain(e.target.value)}
            rows={6}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            disabled={upload.isPending}
            className="font-mono text-xs md:text-xs"
          />
        </Field>
        <Field>
          <FieldLabel htmlFor="upload-cert-key">{t("ingressPage.certs.upload.key")}</FieldLabel>
          <Textarea
            id="upload-cert-key"
            value={key}
            onChange={(e) => setKey(e.target.value)}
            rows={6}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            disabled={upload.isPending}
            className="font-mono text-xs md:text-xs"
            aria-describedby="upload-cert-key-helper"
          />
          <FieldDescription id="upload-cert-key-helper">
            {t("ingressPage.certs.upload.helper")}
          </FieldDescription>
        </Field>
      </FieldGroup>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={upload.isPending}
          onClick={onClose}
        >
          {t("ingressPage.certs.upload.discard")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={!ready || upload.isPending}>
          {upload.isPending ? <Spinner aria-hidden="true" /> : null}
          {t("ingressPage.certs.upload.submit")}
        </Button>
      </DialogFooter>
    </form>
  );
}

// SwitchToACMEDialog confirms returning an uploaded hostname to Let's Encrypt (D-12, UI-SPEC S3, U-20). Nothing is
// sent until the confirm is pressed, and the dialog stays open until the server answers, so a refusal reads in place.
export function SwitchToACMEDialog({
  envId,
  cert,
  open,
  onOpenChange,
  returnFocusRef,
}: {
  envId: string;
  cert: IngressCert;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  returnFocusRef?: RefObject<HTMLElement | null>;
}) {
  const t = useT();
  const sw = useSwitchCertificateToACME(envId);
  const host = cert.hostname;

  function change(next: boolean) {
    if (!next && sw.isPending) return;
    if (next) sw.reset();
    onOpenChange(next);
  }

  return (
    <AlertDialog open={open} onOpenChange={change}>
      <AlertDialogContent onCloseAutoFocus={focusBack(returnFocusRef)}>
        <AlertDialogHeader>
          <AlertDialogTitle className="break-words">
            {t("ingressPage.certs.switch.title", { host })}
          </AlertDialogTitle>
          <AlertDialogDescription>{t("ingressPage.certs.switch.body")}</AlertDialogDescription>
        </AlertDialogHeader>
        {sw.isError ? <Refusal error={sw.error} /> : null}
        <AlertDialogFooter>
          {/* Radix focuses the dismiss button when the dialog opens. */}
          <AlertDialogCancel disabled={sw.isPending} className={TOUCH_HEIGHT}>
            {t("ingressPage.certs.switch.keep")}
          </AlertDialogCancel>
          <Button
            type="button"
            className={TOUCH_HEIGHT}
            disabled={sw.isPending}
            onClick={() =>
              // Radix would close the dialog on a click; it stays open until the request answers.
              sw.mutate(cert.id, {
                onSuccess: () => {
                  toastSuccess(t("ingressPage.certs.switch.done", { host }));
                  onOpenChange(false);
                },
              })
            }
          >
            {sw.isPending ? <Spinner aria-hidden="true" /> : null}
            {t("ingressPage.certs.switch.confirm")}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
