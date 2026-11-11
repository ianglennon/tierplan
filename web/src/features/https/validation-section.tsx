// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { Info } from "lucide-react";
import { type FormEvent, useEffect, useState } from "react";
import { useForm, useWatch } from "react-hook-form";

import {
  type ACMETokenTest,
  ACMEValidationError,
  type HTTPSACME,
  useSaveACMEValidation,
  useTestACMEDNSToken,
} from "@/api/https";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription } from "@/components/ui/field";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import type { ValidationMode } from "@/features/https/card-state";
import { DnsSettings, TestTokenButton, TestTokenNotes } from "@/features/https/dns-settings";
import { Sentence, SLOT } from "@/features/https/sentence";
import { TokenSavedRow } from "@/features/https/token-saved-row";
import { TokenTestResult } from "@/features/https/token-test-result";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { z } from "@/lib/zod-config";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

const METHODS = ["http-01", "dns-01"] as const;
type Method = (typeof METHODS)[number];

// The draft: the method, the token being typed and whether it is typed over a saved one. Validation
// here is UX only; the server checks again. The token is in this form and nowhere else.
const TOKEN_REQUIRED = "https.dns.token.required";

// DNS-01 needs a token to send when none is saved, or when the person chose to replace the saved one.
function makeSchema(tokenSaved: boolean) {
  return z
    .object({
      challengeType: z.enum(METHODS),
      token: z.string(),
      replacing: z.boolean(),
    })
    .superRefine((v, ctx) => {
      if (v.challengeType === "dns-01" && (!tokenSaved || v.replacing) && v.token.trim() === "") {
        ctx.addIssue({ code: "custom", path: ["token"], message: TOKEN_REQUIRED });
      }
    });
}

type Values = z.infer<ReturnType<typeof makeSchema>>;

const HEADING_ID = "acme-validation-title";

// What a save or a Test token answered, to show under the buttons: the token test it ran, and whether it
// stored (null for Test token, which never stores).
interface Outcome {
  test: ACMETokenTest;
  saved: boolean | null;
}

interface ValidationSectionProps {
  acme: HTTPSACME;
  mode: ValidationMode;
  // Told whether the draft differs from what is saved, so the card can keep Issue and Renew from
  // ordering with settings the page is not showing (UI-SPEC J4).
  onDirtyChange?: (dirty: boolean) => void;
}

// ValidationSection is "Validation method" on the Let's Encrypt card (CHAL-01, CHAL-02, D-02, D-06,
// D-07, UI-SPEC J2): the challenge picker, the detail of the chosen method and one Save. Picking a radio
// changes the draft only; nothing is stored until Save, which is where a new token is tested first.
export function ValidationSection({ acme, mode, onDirtyChange }: ValidationSectionProps) {
  const t = useT();
  const save = useSaveACMEValidation();
  const test = useTestACMEDNSToken();
  const saved: Method = acme.challengeType === "dns-01" ? "dns-01" : "http-01";
  const tokenSaved = acme.dns.tokenSaved;
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // "testing" while the server tests a typed token, "saving" from its answer until the status is read.
  const [stage, setStage] = useState<"testing" | "saving">("saving");
  const [failed, setFailed] = useState(false);

  const form = useForm<Values>({
    resolver: zodResolver(makeSchema(tokenSaved)),
    values: { challengeType: saved, token: "", replacing: false },
    mode: "onSubmit",
  });
  const method = useWatch({ control: form.control, name: "challengeType" });
  const replacing = useWatch({ control: form.control, name: "replacing" }) && tokenSaved;
  const typed = useWatch({ control: form.control, name: "token" }).trim() !== "";
  const saving = save.isPending;
  const testing = test.isPending;
  const pending = saving || testing;
  const editable = mode === "editable";
  // Under HTTP-01 only the method can differ: leaving DNS-01 drops every other draft value (J9).
  const dirty = method !== saved || (method === "dns-01" && (typed || replacing));
  const locked = pending || !editable;

  useEffect(() => {
    onDirtyChange?.(dirty);
    return () => onDirtyChange?.(false);
  }, [dirty, onDirtyChange]);

  // Picking a radio changes the draft and nothing else. Leaving DNS-01 discards what was typed for it:
  // the token, replacing and the result, so no hidden value is left behind (J9, D-05).
  const pick = (value: string) => {
    const next: Method = value === "dns-01" ? "dns-01" : "http-01";
    form.setValue("challengeType", next);
    if (next === "http-01") {
      form.setValue("token", "");
      form.setValue("replacing", false);
      form.clearErrors();
      setOutcome(null);
    }
  };

  const host = acme.publicHost === "" ? t("https.acme.publicHostFallback") : acme.publicHost;
  const flag = <code className="font-mono text-xs">{`-p 80:${acme.listenPort}`}</code>;

  // Test token: the typed token, or else the saved one, with nothing stored. With neither there is
  // nothing to test, and nothing is sent.
  const runTest = () => {
    const token = form.getValues("token").trim();
    if (token === "" && !tokenSaved) {
      form.setError("token", { type: "test", message: "https.dns.test.required" });
      return;
    }
    form.clearErrors("token");
    setFailed(false);
    setOutcome(null);
    test.mutate(token === "" ? {} : { token }, {
      onSuccess: (answer) => setOutcome({ test: answer, saved: null }),
      onError: () => setFailed(true),
    });
  };

  const send = form.handleSubmit((v) => {
    setFailed(false);
    setOutcome(null);
    const token = v.challengeType === "dns-01" ? v.token.trim() : "";
    setStage(token === "" ? "saving" : "testing");
    save.mutate(
      {
        challengeType: v.challengeType,
        ...(token === "" ? {} : { token }),
        onAnswered: () => setStage("saving"),
      },
      {
        onSuccess: (answer) => {
          setOutcome(answer.test ? { test: answer.test, saved: answer.saved } : null);
          if (!answer.saved) return;
          // Stored: the typed token leaves the form, and the status shows only that one is saved.
          form.reset({ challengeType: v.challengeType, token: "", replacing: false });
          toastSuccess(t("https.validation.done"));
        },
        onError: (error) => {
          if (error instanceof ACMEValidationError && error.type === "acme_dns_token_required") {
            form.setError("token", { type: "server", message: TOKEN_REQUIRED });
            return;
          }
          setFailed(true);
        },
      },
    );
  });
  // Save does nothing while nothing differs or a save is on its way, as Enter in the field would
  // otherwise reach it past the aria-disabled button.
  const submit = (event: FormEvent<HTMLFormElement>) => {
    if (!dirty || locked) {
      event.preventDefault();
      return;
    }
    void send(event);
  };
  const tokenMessage = form.formState.errors.token?.message;

  return (
    <section aria-labelledby={HEADING_ID} className="flex flex-col gap-4">
      <h4 id={HEADING_ID} className="text-sm font-semibold">
        {t("https.acme.method.label")}
      </h4>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <fieldset disabled={locked} className="contents">
          <RadioGroup
            value={method}
            onValueChange={pick}
            disabled={locked}
            aria-labelledby={HEADING_ID}
            className="gap-4"
          >
            {METHODS.map((m) => (
              <Field key={m} orientation="horizontal" className="items-start gap-3">
                <RadioGroupItem
                  value={m}
                  id={`acme-method-${m}`}
                  aria-describedby={`acme-method-${m}-description`}
                  className="mt-0.5"
                />
                <div className="flex min-w-0 flex-col gap-1">
                  <Label
                    htmlFor={`acme-method-${m}`}
                    className="font-semibold max-md:min-h-11 max-md:items-start"
                  >
                    {t(
                      m === "dns-01"
                        ? "https.validation.dns01.label"
                        : "https.validation.http01.label",
                    )}
                  </Label>
                  <FieldDescription id={`acme-method-${m}-description`} className="break-words">
                    {t(
                      m === "dns-01"
                        ? "https.validation.dns01.description"
                        : "https.validation.http01.description",
                    )}
                  </FieldDescription>
                </div>
              </Field>
            ))}
          </RadioGroup>
          <p className="text-sm text-muted-foreground">{t("https.validation.helper")}</p>

          {method === "http-01" ? (
            <>
              <Alert role="note" className="text-foreground">
                <Info aria-hidden="true" />
                <AlertDescription className="text-foreground">
                  <Sentence text={t("https.acme.portNote", { host, flag: SLOT })} slot={flag} />
                </AlertDescription>
              </Alert>
              {tokenSaved ? <TokenSavedRow kept /> : null}
            </>
          ) : (
            <DnsSettings
              tokenSaved={tokenSaved}
              replacing={replacing}
              editable={editable}
              tokenField={form.register("token")}
              tokenError={typeof tokenMessage === "string" ? t(tokenMessage) : undefined}
              onReplace={() => form.setValue("replacing", true)}
              onKeep={() => {
                form.setValue("replacing", false);
                form.setValue("token", "");
                form.clearErrors("token");
              }}
            />
          )}

          {failed ? (
            <Alert variant="destructive">
              <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
            </Alert>
          ) : null}

          {editable ? (
            <div className="flex flex-wrap gap-2">
              {method === "dns-01" ? (
                <TestTokenButton busy={pending} testing={testing} onTest={runTest} />
              ) : null}
              <Button
                type="submit"
                variant="outline"
                aria-disabled={!dirty || pending}
                className={`${TOUCH_HEIGHT} max-md:w-full aria-disabled:cursor-not-allowed aria-disabled:opacity-50`}
              >
                {saving ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t(
                      stage === "testing" ? "https.validation.testing" : "https.validation.saving",
                    )}
                  </>
                ) : (
                  t("https.validation.save")
                )}
              </Button>
            </div>
          ) : null}

          {editable && method === "dns-01" ? <TestTokenNotes pending={testing} /> : null}

          {outcome ? (
            <TokenTestResult result={outcome.test} notSaved={outcome.saved === false} />
          ) : null}
        </fieldset>
      </form>
    </section>
  );
}
