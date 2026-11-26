// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { type FormEvent, useRef, useState } from "react";

import { IngressActionError, type IngressDomain } from "@/api/ingress";
import { type DomainIssuer, useChangeIssuer, useIssuerPreview } from "@/api/ingress-domains";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Field, FieldLabel } from "@/components/ui/field";
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

// Refusal is a write the server refused, in the server's own words: the detail it sent, as sent, in a destructive
// alert that stays in the dialog and never in a toast. A failure that carries no detail (the network, a proxy) gets the
// common sentence.
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

// useShownDomain hands back the domain a dialog was opened for until it has closed, so the title does not lose the
// name while the dialog fades out.
function useShownDomain(domain: IngressDomain | null): IngressDomain | null {
  const shown = useRef<IngressDomain | null>(null);
  if (domain) shown.current = domain;
  return shown.current;
}

interface DomainDialogProps {
  envId: string;
  // The domain the dialog is for. Null while the dialog is closed.
  domain: IngressDomain | null;
  onClose: () => void;
}

// ChangeIssuerDialog changes the Let's Encrypt directory a domain's certificates come from (D-07, UI-SPEC S5). It
// previews first: once a directory is chosen the dialog reads what the change would do (the certificates ordered
// again and the uploads left alone) and the primary button waits for that. The change sends the preview's count back,
// and when a certificate came or went in between the server's 409 reads in the dialog and the preview is read again,
// so the next press confirms the new number. The form is mounted only while the dialog is open.
export function ChangeIssuerDialog({ envId, domain, onClose }: DomainDialogProps) {
  const shown = useShownDomain(domain);
  return (
    <Dialog open={domain !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent aria-describedby={undefined}>
        {shown ? <ChangeIssuerForm envId={envId} domain={shown} onClose={onClose} /> : null}
      </DialogContent>
    </Dialog>
  );
}

function ChangeIssuerForm({
  envId,
  domain,
  onClose,
}: {
  envId: string;
  domain: IngressDomain;
  onClose: () => void;
}) {
  const t = useT();
  const change = useChangeIssuer(envId);
  const [issuer, setIssuer] = useState<DomainIssuer | null>(null);
  const preview = useIssuerPreview(envId, domain.id, issuer);
  const sending = useRef(false);
  const name = (i: DomainIssuer) => t(`ingressPage.domains.add.${i}`);

  function choose(next: string) {
    change.reset();
    setIssuer(next === "staging" ? "staging" : "production");
  }

  function submit(event: FormEvent) {
    event.preventDefault();
    if (!issuer || !preview.data || change.isPending || sending.current) return;
    sending.current = true;
    change.mutate(
      { domainId: domain.id, issuer, confirmCount: preview.data.confirmCount },
      {
        onSettled: () => {
          sending.current = false;
        },
        onSuccess: () => {
          toastSuccess(
            t("ingressPage.domains.changeIssuer.done", {
              suffix: domain.suffix,
              issuer: name(issuer),
            }),
          );
          onClose();
        },
        // The count is out of date: read it again so the person confirms what is true now.
        onError: () => void preview.refetch(),
      },
    );
  }

  const data = preview.data;
  const uploads = data?.uploads ?? [];
  const failure = change.isError ? change.error : preview.isError ? preview.error : undefined;

  return (
    <form noValidate onSubmit={submit} className="flex flex-col gap-4">
      <DialogHeader>
        <DialogTitle className="break-words">
          {t("ingressPage.domains.changeIssuer.title", { suffix: domain.suffix })}
        </DialogTitle>
      </DialogHeader>
      {failure ? <Refusal error={failure} /> : null}
      <Field>
        <FieldLabel htmlFor="ingress-change-issuer">
          {t("ingressPage.domains.changeIssuer.field")}
        </FieldLabel>
        <Select value={issuer ?? ""} onValueChange={choose} disabled={change.isPending}>
          <SelectTrigger id="ingress-change-issuer" className="w-full max-md:h-11">
            <SelectValue placeholder={t("ingressPage.domains.changeIssuer.placeholder")} />
          </SelectTrigger>
          <SelectContent>
            {(["production", "staging"] as const).map((i) => (
              <SelectItem key={i} value={i} disabled={i === domain.issuer}>
                {i === domain.issuer
                  ? t("ingressPage.domains.changeIssuer.current", { name: name(i) })
                  : name(i)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </Field>
      {issuer && preview.isPending ? (
        <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
          <Spinner aria-hidden="true" />
          {t("ingressPage.domains.changeIssuer.loading")}
        </p>
      ) : null}
      {data ? (
        <div className="flex flex-col gap-2 text-sm">
          <p>
            {t("ingressPage.domains.changeIssuer.paragraph", {
              suffix: domain.suffix,
              issuer: name(data.issuer),
            })}
          </p>
          <p className="font-medium">
            {t("ingressPage.domains.changeIssuer.count", { count: data.certificates })}
          </p>
          {uploads.length > 0 ? (
            <>
              <p>{t("ingressPage.domains.changeIssuer.uploads")}</p>
              <ul className="flex flex-col gap-1">
                {uploads.map((u) => (
                  <li key={u} className="font-mono text-xs break-all">
                    {u}
                  </li>
                ))}
              </ul>
            </>
          ) : null}
        </div>
      ) : null}
      <DialogFooter>
        <Button
          type="button"
          variant="outline"
          className={TOUCH_HEIGHT}
          disabled={change.isPending}
          onClick={onClose}
        >
          {t("ingressPage.domains.changeIssuer.discard")}
        </Button>
        <Button type="submit" className={TOUCH_HEIGHT} disabled={!data || change.isPending}>
          {change.isPending ? <Spinner aria-hidden="true" /> : null}
          {t("ingressPage.domains.changeIssuer.submit")}
        </Button>
      </DialogFooter>
    </form>
  );
}
