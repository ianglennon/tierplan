// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { ChevronRight, Eye, EyeOff, Info } from "lucide-react";
import { useState } from "react";
import type { UseFormRegisterReturn } from "react-hook-form";

import type { HTTPSACME } from "@/api/https";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupButton,
  InputGroupInput,
} from "@/components/ui/input-group";
import { Spinner } from "@/components/ui/spinner";
import { LockedIcon, LockedNote } from "@/features/https/locked-note";
import { rich } from "@/features/https/sentence";
import { TokenLockBlock, TokenSavedRow } from "@/features/https/token-saved-row";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

const CLOUDFLARE_TOKENS_URL = "https://dash.cloudflare.com/profile/api-tokens";
export const TOKEN_ID = "acme-dns-token";
export const RESOLVERS_ID = "acme-dns-resolvers";
export const PROVIDER_LOCK_ID = "acme-dns-provider-lock";
export const RESOLVERS_LOCK_ID = "acme-dns-resolvers-lock";

// The cloudflare id as the page names it; any other id the server sends is shown as it is.
function useProviderName() {
  const t = useT();
  return (id: string) => (id === "cloudflare" ? t("https.dns.provider.cloudflare") : id);
}

// The DNS locks of a status (D-03, D-05, D-17). The provider and its token lock together: either entry
// fixes both, so the page never offers a token field beside a provider the environment names. tokenVar
// and providerVar are always the API's, so the label names the variable in use.
export function dnsLocks(locks: HTTPSACME["locks"]) {
  const provider = locks.dnsProvider;
  const token = locks.dnsToken;
  const tokenLocked = provider !== undefined || token !== undefined;
  return {
    tokenLocked,
    provider,
    providerVar: provider?.var ?? token?.var ?? "",
    tokenVar: token?.var ?? provider?.var ?? "",
    resolvers: locks.resolvers,
  };
}

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
  // The registration of the resolvers field (a comma-separated list), and the Advanced DNS disclosure
  // that holds it. The section owns the open state so a server error can open it.
  resolversField: UseFormRegisterReturn<"resolvers">;
  // The resolver the server refused. entry is what it named, when it named one.
  resolversProblem?: { entry?: string };
  // True while a save or a test is on its way: the field is aria-disabled.
  busy: boolean;
  advancedOpen: boolean;
  onAdvancedOpenChange: (open: boolean) => void;
  // What the saved token is saved for, and how many test records TierPlan failed to delete: the Remove
  // token button and its dialog (D-05, D-18).
  savedChallenge: "http-01" | "dns-01";
  leftOverCount: number;
  // The locks the environment puts on the provider, the token and the resolvers.
  locks: HTTPSACME["locks"];
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
  resolversField,
  resolversProblem,
  busy,
  advancedOpen,
  onAdvancedOpenChange,
  savedChallenge,
  leftOverCount,
  locks,
}: DnsSettingsProps) {
  const t = useT();
  const providerName = useProviderName();
  const lock = dnsLocks(locks);
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
        <dd className="flex min-w-0 flex-col gap-1 break-words text-sm">
          {providerName(lock.provider?.value ?? "cloudflare")}
          {lock.tokenLocked ? (
            <LockedNote
              id={PROVIDER_LOCK_ID}
              lock={{ var: lock.providerVar, stored: lock.provider?.stored }}
              storedLabel={lock.provider?.stored ? providerName(lock.provider.stored) : undefined}
            />
          ) : null}
        </dd>
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

      {lock.tokenLocked ? (
        <TokenLockBlock varName={lock.tokenVar} saved={tokenSaved} />
      ) : tokenSaved && !replacing ? (
        <TokenSavedRow
          onReplace={editable ? onReplace : undefined}
          savedChallenge={savedChallenge}
          leftOverCount={leftOverCount}
          canRemove={editable}
        />
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

      <AdvancedDns
        open={advancedOpen}
        onOpenChange={onAdvancedOpenChange}
        field={resolversField}
        problem={resolversProblem}
        busy={busy}
        lock={lock.resolvers}
      />
    </div>
  );
}

interface AdvancedDnsProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  field: UseFormRegisterReturn<"resolvers">;
  problem?: { entry?: string };
  busy: boolean;
  // TIERPLAN_DNS_RESOLVERS fixes the list: the field is read-only and shows the effective list.
  lock?: HTTPSACME["locks"]["resolvers"];
}

// AdvancedDns is the disclosure that holds the optional resolver list (CHAL-05, D-12, D-14). There is
// no switch to skip the propagation check: the list only chooses who is asked.
function AdvancedDns({ open, onOpenChange, field, problem, busy, lock }: AdvancedDnsProps) {
  const t = useT();
  const error = problem
    ? problem.entry
      ? rich(t, "https.dns.resolvers.error", { entry: problem.entry })
      : t("https.dns.resolvers.errorList")
    : null;
  return (
    <Collapsible open={open} onOpenChange={onOpenChange} className="flex flex-col gap-4">
      <CollapsibleTrigger asChild>
        <Button variant="ghost" size="sm" className="self-start max-md:h-11">
          <ChevronRight
            aria-hidden="true"
            className={cn(
              "transition-transform motion-reduce:transition-none",
              open && "rotate-90",
            )}
          />
          {t("https.dns.advanced.label")}
        </Button>
      </CollapsibleTrigger>
      <CollapsibleContent className="flex flex-col gap-2">
        <Field data-invalid={error ? true : undefined}>
          <FieldLabel htmlFor={RESOLVERS_ID}>{t("https.dns.resolvers.label")}</FieldLabel>
          {lock ? (
            <>
              <InputGroup className={TOUCH_HEIGHT}>
                <InputGroupAddon>
                  <LockedIcon />
                </InputGroupAddon>
                <InputGroupInput
                  id={RESOLVERS_ID}
                  type="text"
                  readOnly
                  value={lock.value ?? ""}
                  placeholder={t("https.dns.resolvers.lockedDefault")}
                  autoComplete="off"
                  aria-describedby={`${RESOLVERS_LOCK_ID} ${RESOLVERS_ID}-helper`}
                  className="h-full min-w-0 font-mono"
                />
              </InputGroup>
              <LockedNote id={RESOLVERS_LOCK_ID} lock={lock} showIcon={false} mono />
            </>
          ) : (
            <Input
              id={RESOLVERS_ID}
              type="text"
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              placeholder={t("https.dns.resolvers.placeholder")}
              aria-invalid={Boolean(error)}
              aria-disabled={busy}
              aria-describedby={`${RESOLVERS_ID}-helper${error ? ` ${RESOLVERS_ID}-error` : ""}`}
              className={`${TOUCH_HEIGHT} font-mono`}
              {...field}
            />
          )}
          <FieldDescription id={`${RESOLVERS_ID}-helper`}>
            {t("https.dns.resolvers.helper")}
          </FieldDescription>
          {error ? (
            <FieldError id={`${RESOLVERS_ID}-error`} className="break-words">
              {error}
            </FieldError>
          ) : null}
        </Field>
        <p className="text-sm text-muted-foreground">
          {rich(t, "https.dns.resolvers.delegation", { name: "_acme-challenge.<name>" })}
        </p>
      </CollapsibleContent>
    </Collapsible>
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

interface TestTokenButtonProps {
  // True while a token test or a save is on its way: the button is aria-disabled and does nothing.
  busy: boolean;
  // True while this button's own test is the thing in flight: it shows the spinner.
  testing: boolean;
  onTest: () => void;
}

// TestTokenButton is "Test token" (CHAL-02, D-07): a button of its own beside Save. It tests the typed
// token, or else the saved one, and stores nothing.
export function TestTokenButton({ busy, testing, onTest }: TestTokenButtonProps) {
  const t = useT();
  return (
    <Button
      type="button"
      variant="outline"
      aria-disabled={busy}
      className={`${TOUCH_HEIGHT} max-md:w-full aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
      onClick={() => {
        if (!busy) onTest();
      }}
    >
      {testing ? (
        <>
          <Spinner aria-hidden="true" />
          {t("https.dns.test.pending")}
        </>
      ) : (
        t("https.dns.test.action")
      )}
    </Button>
  );
}

// TestTokenNotes is the text under the button row: what a test does, and while one runs that it can
// take a minute (UI-SPEC J8).
export function TestTokenNotes({
  pending,
  tokenVar,
}: {
  pending: boolean;
  // The variable that supplies the token, when one does: the helper then names it, since Test token
  // tests the environment's token (D-05).
  tokenVar?: string;
}) {
  const t = useT();
  return (
    <>
      <p className="text-sm text-muted-foreground">
        {tokenVar
          ? rich(t, "https.dns.test.helperEnv", { var: tokenVar })
          : t("https.dns.test.helper")}
      </p>
      {pending ? <p className="text-xs text-muted-foreground">{t("https.dns.test.hint")}</p> : null}
    </>
  );
}
