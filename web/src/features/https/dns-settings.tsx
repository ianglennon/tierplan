// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Eye, EyeOff, Info } from "lucide-react";
import { useState } from "react";
import type { UseFormRegisterReturn } from "react-hook-form";

import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { rich } from "@/features/https/sentence";
import { TokenSavedRow } from "@/features/https/token-saved-row";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

const CLOUDFLARE_TOKENS_URL = "https://dash.cloudflare.com/profile/api-tokens";
const TOKEN_ID = "acme-dns-token";

interface DnsSettingsProps {
  // True when a token is saved. Only this is known: the token itself is never sent to the page.
  tokenSaved: boolean;
  // True while the person is typing a new token over the saved one.
  replacing: boolean;
  // False when the section is disabled: the saved token is shown, with nothing to press.
  editable: boolean;
  // The react-hook-form registration of the token field. The typed value lives in the form and nowhere
  // else, and the form clears it when a save succeeds.
  tokenField: UseFormRegisterReturn<"token">;
  tokenError?: string;
  onReplace: () => void;
  onKeep: () => void;
}

// DnsSettings is the DNS-01 detail of the Validation section (CHAL-01, D-02, D-06, UI-SPEC J1): the
// provider as a labelled value, how to make the token, and the write-only token field or, once a token
// is saved, the row that says so.
export function DnsSettings({
  tokenSaved,
  replacing,
  editable,
  tokenField,
  tokenError,
  onReplace,
  onKeep,
}: DnsSettingsProps) {
  const t = useT();
  const link = (
    <a
      href={CLOUDFLARE_TOKENS_URL}
      target="_blank"
      rel="noopener noreferrer"
      className="text-accent-ink underline underline-offset-2"
    >
      {t("https.dns.setup.link")}
    </a>
  );

  return (
    <div className="flex flex-col gap-4">
      <dl className="grid grid-cols-1 gap-x-6 gap-y-4 md:grid-cols-[160px_1fr]">
        <dt className="text-sm font-medium text-muted-foreground">
          {t("https.dns.provider.label")}
        </dt>
        <dd className="min-w-0 break-words text-sm">{t("https.dns.provider.cloudflare")}</dd>
      </dl>

      <Alert role="note" className="text-foreground">
        <Info aria-hidden="true" />
        <AlertDescription className="flex flex-col gap-2 text-foreground">
          <p>
            {rich(t, "https.dns.setup.intro", {
              edit: t("https.dns.setup.permEdit"),
              read: t("https.dns.setup.permRead"),
              link,
            })}
          </p>
          <p>{t("https.dns.setup.records")}</p>
        </AlertDescription>
      </Alert>

      {tokenSaved && !replacing ? (
        <TokenSavedRow onReplace={editable ? onReplace : undefined} />
      ) : (
        <div className="flex flex-col gap-2">
          <TokenField
            field={tokenField}
            label={t(tokenSaved ? "https.dns.token.labelNew" : "https.dns.token.label")}
            error={tokenError}
          />
          {tokenSaved && editable ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="self-start max-md:h-11 max-md:w-full"
              onClick={onKeep}
            >
              {t("https.dns.token.keep")}
            </Button>
          ) : null}
        </div>
      )}
    </div>
  );
}

interface TokenFieldProps {
  field: UseFormRegisterReturn<"token">;
  label: string;
  error?: string;
}

// TokenField is the write-only secret field of the registries sheet: a password input with a show and
// hide button, no autofill, no capitals, no spell check. Its own state is the show flag, so it starts
// hidden every time the field appears.
function TokenField({ field, label, error }: TokenFieldProps) {
  const t = useT();
  const [show, setShow] = useState(false);
  return (
    <Field data-invalid={error ? true : undefined}>
      <FieldLabel htmlFor={TOKEN_ID}>{label}</FieldLabel>
      <InputGroup className={TOUCH_HEIGHT}>
        <InputGroupInput
          id={TOKEN_ID}
          type={show ? "text" : "password"}
          autoComplete="new-password"
          autoCapitalize="off"
          autoCorrect="off"
          spellCheck={false}
          aria-invalid={Boolean(error)}
          aria-describedby={error ? `${TOKEN_ID}-error` : `${TOKEN_ID}-helper`}
          className="min-w-0"
          {...field}
        />
        <InputGroupAddon align="inline-end">
          <InputGroupButton
            size="icon-xs"
            className="max-md:size-11"
            aria-pressed={show}
            aria-label={t(show ? "https.dns.token.hide" : "https.dns.token.show")}
            onClick={() => setShow((shown) => !shown)}
          >
            {show ? <EyeOff aria-hidden="true" /> : <Eye aria-hidden="true" />}
          </InputGroupButton>
        </InputGroupAddon>
      </InputGroup>
      <FieldDescription id={`${TOKEN_ID}-helper`}>{t("https.dns.token.helper")}</FieldDescription>
      {error ? <FieldError id={`${TOKEN_ID}-error`}>{error}</FieldError> : null}
    </Field>
  );
}
