// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { zodResolver } from "@hookform/resolvers/zod";
import { Info } from "lucide-react";
import { useState } from "react";
import { useForm, useWatch } from "react-hook-form";

import { type ACMETokenTest, type HTTPSACME, useSaveACMEValidation } from "@/api/https";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Field, FieldDescription } from "@/components/ui/field";
import { Label } from "@/components/ui/label";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Spinner } from "@/components/ui/spinner";
import type { ValidationMode } from "@/features/https/card-state";
import { DnsSettings } from "@/features/https/dns-settings";
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
const schema = z.object({
  challengeType: z.enum(METHODS),
  token: z.string(),
  replacing: z.boolean(),
});

type Values = z.infer<typeof schema>;

const HEADING_ID = "acme-validation-title";

// What a save answered, to show under the buttons: the token test it ran, and whether it stored.
interface Outcome {
  test: ACMETokenTest;
  saved: boolean;
}

interface ValidationSectionProps {
  acme: HTTPSACME;
  mode: ValidationMode;
}

// ValidationSection is "Validation method" on the Let's Encrypt card (CHAL-01, CHAL-02, D-02, D-06,
// D-07, UI-SPEC J2): the challenge picker, the detail of the chosen method and one Save. Picking a radio
// changes the draft only; nothing is stored until Save, which is where a new token is tested first.
export function ValidationSection({ acme, mode }: ValidationSectionProps) {
  const t = useT();
  const save = useSaveACMEValidation();
  const saved: Method = acme.challengeType === "dns-01" ? "dns-01" : "http-01";
  const tokenSaved = acme.dns.tokenSaved;
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // "testing" while the server tests a typed token, "saving" from its answer until the status is read.
  const [stage, setStage] = useState<"testing" | "saving">("saving");
  const [failed, setFailed] = useState(false);

  const form = useForm<Values>({
    resolver: zodResolver(schema),
    values: { challengeType: saved, token: "", replacing: false },
    mode: "onSubmit",
  });
  const method = useWatch({ control: form.control, name: "challengeType" });
  const replacing = useWatch({ control: form.control, name: "replacing" }) && tokenSaved;
  const pending = save.isPending;
  const editable = mode === "editable";

  const host = acme.publicHost === "" ? t("https.acme.publicHostFallback") : acme.publicHost;
  const flag = <code className="font-mono text-xs">{`-p 80:${acme.listenPort}`}</code>;

  const submit = form.handleSubmit((v) => {
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
        onError: () => setFailed(true),
      },
    );
  });

  return (
    <section aria-labelledby={HEADING_ID} className="flex flex-col gap-4">
      <h4 id={HEADING_ID} className="text-sm font-semibold">
        {t("https.acme.method.label")}
      </h4>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <fieldset disabled={pending || !editable} className="contents">
          <RadioGroup
            value={method}
            onValueChange={(v) =>
              form.setValue("challengeType", v === "dns-01" ? "dns-01" : "http-01")
            }
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
              onReplace={() => form.setValue("replacing", true)}
              onKeep={() => {
                form.setValue("replacing", false);
                form.setValue("token", "");
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
              <Button type="submit" variant="outline" className={`${TOUCH_HEIGHT} max-md:w-full`}>
                {pending ? (
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

          {outcome ? <TokenTestResult result={outcome.test} notSaved={!outcome.saved} /> : null}
        </fieldset>
      </form>
    </section>
  );
}
