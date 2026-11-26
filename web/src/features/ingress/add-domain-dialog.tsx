// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type FormEvent, useId, useRef, useState } from "react";

import { IngressActionError } from "@/api/ingress";
import { useCreateDomain } from "@/api/ingress-domains";
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

type Issuer = "production" | "staging";

// AddDomainDialog registers a domain with its DNS token (UIX-05, UI-SPEC S5). The server tests the token before it
// saves anything, so the button shows a Spinner while it does and a failed test reads in the dialog, as the server
// worded it, with nothing saved. The token is the one secret this dialog holds: it lives only in the form's state, and
// the form is mounted only while the dialog is open, so closing it, on success or on Discard domain, drops it. It is
// never echoed after the request, never put in a toast and never in a live region (T-27-64).
export function AddDomainDialog({
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
        <AddDomainForm envId={envId} onClose={() => onOpenChange(false)} />
      </DialogContent>
    </Dialog>
  );
}

function AddDomainForm({ envId, onClose }: { envId: string; onClose: () => void }) {
  const t = useT();
  const create = useCreateDomain(envId);
  const ids = useId();
  const [suffix, setSuffix] = useState("");
  const [issuer, setIssuer] = useState<Issuer>("production");
  const [token, setToken] = useState("");
  // Set the moment a request leaves, ahead of the render that disables the button, so a double press sends one.
  const sending = useRef(false);
  const domain = suffix.trim();
  const ready = domain !== "" && token !== "";

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!ready || create.isPending || sending.current) return;
    sending.current = true;
    create.mutate(
      { suffix: domain, issuer, token },
      {
        onSettled: () => {
          sending.current = false;
        },
        onSuccess: () => {
          toastSuccess(t("ingressPage.domains.add.done", { suffix: domain }));
          onClose();
        },
      },
    );
  }

  const reason = create.error instanceof IngressActionError ? create.error.detail : undefined;

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle>{t("ingressPage.domains.add.title")}</DialogTitle>
      </DialogHeader>
      {create.isError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="break-words">
            {reason ?? t("common.mutationFailed")}
          </AlertDescription>
        </Alert>
      ) : null}
      <FieldGroup className="gap-4">
        <Field>
          <FieldLabel htmlFor={`${ids}-suffix`}>{t("ingressPage.domains.add.domain")}</FieldLabel>
          <Input
            id={`${ids}-suffix`}
            value={suffix}
            onChange={(e) => setSuffix(e.target.value)}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            disabled={create.isPending}
            className={`${TOUCH_HEIGHT} font-mono text-xs`}
            aria-describedby={`${ids}-suffix-helper`}
          />
          <FieldDescription id={`${ids}-suffix-helper`}>
            {t("ingressPage.domains.add.domainHelper")}
          </FieldDescription>
        </Field>
        <Field>
          <FieldLabel htmlFor={`${ids}-issuer`}>{t("ingressPage.domains.add.issuer")}</FieldLabel>
          <Select
            value={issuer}
            onValueChange={(next) => setIssuer(next === "staging" ? "staging" : "production")}
            disabled={create.isPending}
          >
            <SelectTrigger id={`${ids}-issuer`} className="w-full max-md:h-11">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="production">{t("ingressPage.domains.add.production")}</SelectItem>
              <SelectItem value="staging">{t("ingressPage.domains.add.staging")}</SelectItem>
            </SelectContent>
          </Select>
          {issuer === "staging" ? (
            <FieldDescription>{t("ingressPage.domains.add.stagingHelper")}</FieldDescription>
          ) : null}
        </Field>
        <Field>
          <FieldLabel htmlFor={`${ids}-token`}>{t("ingressPage.domains.add.token")}</FieldLabel>
          <Input
            id={`${ids}-token`}
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            disabled={create.isPending}
            className={TOUCH_HEIGHT}
            aria-describedby={`${ids}-token-helper`}
          />
          <FieldDescription id={`${ids}-token-helper`}>
            {t("ingressPage.domains.add.tokenHelper")}
          </FieldDescription>
        </Field>
      </FieldGroup>
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={create.isPending}
          onClick={onClose}
        >
          {t("ingressPage.domains.add.discard")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={!ready || create.isPending}>
          {create.isPending ? <Spinner aria-hidden="true" /> : null}
          {t("ingressPage.domains.add.submit")}
        </Button>
      </DialogFooter>
    </form>
  );
}
