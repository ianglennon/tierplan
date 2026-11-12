// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { useState } from "react";

import {
  ACMEProfileError,
  acmeDirectoryQuery,
  type HTTPSACME,
  isLockProblem,
  useSaveACMEProfile,
} from "@/api/https";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Field, FieldDescription, FieldLabel } from "@/components/ui/field";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import type { AdvancedMode } from "@/features/https/card-state";
import { LockedNote } from "@/features/https/locked-note";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";
import { cn } from "@/lib/utils";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// A Radix select item cannot have an empty value, so "CA default" is this value in the list and ""
// on the wire. The colons keep it from being the name of a profile (the server accepts none).
const CA_DEFAULT = "::ca-default::";

const toItem = (profile: string) => (profile === "" ? CA_DEFAULT : profile);
const fromItem = (value: string) => (value === CA_DEFAULT ? "" : value);

interface ProfileSectionProps {
  acme: HTTPSACME;
  mode: Exclude<AdvancedMode, "hidden">;
  // False while the card is paused or certificate files are set: the server refuses the directory then.
  listable: boolean;
}

// ProfileSection is "Advanced" on the Let's Encrypt card (ACME-09, UI-SPEC J2): the certificate
// profile, chosen from what the CA's directory offers and saved on its own so a renewal nobody
// starts still knows which profile to ask for. It is closed until opened. Names and descriptions
// come from the CA and are only ever drawn as text.
export function ProfileSection({ acme, mode, listable }: ProfileSectionProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const directory = useQuery({ ...acmeDirectoryQuery, enabled: listable });
  const save = useSaveACMEProfile();
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<string | undefined>();
  const [failed, setFailed] = useState(false);
  // TIERPLAN_ACME_PROFILE fixes the profile (D-24): the select shows the effective one (acme.profile
  // already is it), nothing can be saved, and the lock says which variable and what is kept underneath.
  const lock = acme.locks.profile;
  const disabled = mode === "disabled" || lock !== undefined;

  const saved = acme.profile;
  const choice = draft ?? saved;
  const changed = choice !== saved;
  const offered = directory.data?.profiles ?? [];
  const listRead = directory.isSuccess;
  const unreadable = listable && directory.isError;
  // The list is on its way, or a save and the status read after it are: the choice is not editable.
  const busy = (listable && directory.isPending) || save.isPending;
  // "No profiles" replaces the select. A saved profile the CA no longer offers still needs the select
  // to be put right, so that case keeps it.
  const noProfiles = listRead && offered.length === 0 && saved === "";
  const savedMissing = saved !== "" && listRead && !offered.some((p) => p.name === saved);

  const labelOf = (profile: string) => {
    if (profile === "") return t("https.profile.caDefault");
    if (profile === saved && savedMissing) return t("https.profile.notOffered", { name: profile });
    return profile;
  };

  const submit = () => {
    setFailed(false);
    save.mutate(choice, {
      onSuccess: () => {
        setDraft(undefined);
        toastSuccess(t("https.profile.done"));
      },
      onError: (error) => {
        // A variable now fixes the profile: the status is read again (by the mutation) and the field
        // redraws as locked. The draft was never sent, so it goes, and nothing is told as a failure.
        if (isLockProblem(error)) {
          setDraft(undefined);
          return;
        }
        // The page was out of date: a run began, the card was paused, or the CA dropped the profile.
        if (error instanceof ACMEProfileError && error.status === 409) {
          void queryClient.invalidateQueries({ queryKey: ["https", "status"] });
        }
        if (error instanceof ACMEProfileError && error.status === 422) {
          void queryClient.invalidateQueries({ queryKey: acmeDirectoryQuery.queryKey });
        }
        setFailed(true);
      },
    });
  };

  // The items: the CA default first, then what the directory offers. A saved profile that is not in
  // that list (gone from the CA, or the list could not be read) stays selectable as it was saved.
  const extra = saved !== "" && !offered.some((p) => p.name === saved) ? saved : undefined;

  return (
    <section aria-label={t("https.profile.advanced")} className="flex flex-col gap-4">
      <Collapsible open={open} onOpenChange={setOpen} className="flex flex-col gap-4">
        <CollapsibleTrigger asChild>
          <Button variant="ghost" size="sm" className="self-start max-md:h-11">
            <ChevronRight
              aria-hidden="true"
              className={cn(
                "transition-transform motion-reduce:transition-none",
                open && "rotate-90",
              )}
            />
            {t("https.profile.advanced")}
          </Button>
        </CollapsibleTrigger>
        <CollapsibleContent className="flex flex-col gap-4">
          <Field>
            <FieldLabel htmlFor="acme-profile">{t("https.profile.label")}</FieldLabel>
            {noProfiles ? (
              <p id="acme-profile-none" className="text-sm text-muted-foreground">
                {t("https.profile.none")}
              </p>
            ) : (
              <Select
                value={toItem(choice)}
                onValueChange={(v) => setDraft(fromItem(v))}
                disabled={disabled || busy}
              >
                <SelectTrigger
                  id="acme-profile"
                  title={labelOf(choice)}
                  aria-describedby={
                    lock ? "acme-profile-lock acme-profile-helper" : "acme-profile-helper"
                  }
                  className={cn(TOUCH_HEIGHT, "w-full min-w-0 md:max-w-sm")}
                >
                  <SelectValue>
                    <span className="truncate">{labelOf(choice)}</span>
                  </SelectValue>
                </SelectTrigger>
                <SelectContent className="max-h-72">
                  <SelectItem value={CA_DEFAULT}>{t("https.profile.caDefault")}</SelectItem>
                  {extra === undefined ? null : (
                    <SelectItem value={extra}>
                      <span className="min-w-0 break-words">{labelOf(extra)}</span>
                    </SelectItem>
                  )}
                  {offered.map((p) => (
                    <SelectItem key={p.name} value={p.name}>
                      <span className="flex min-w-0 flex-col items-start gap-0.5">
                        <span className="break-words">{p.name}</span>
                        {p.description === "" ? null : (
                          <span className="break-words text-xs text-muted-foreground">
                            {p.description}
                          </span>
                        )}
                      </span>
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )}
            {lock ? <LockedNote id="acme-profile-lock" lock={lock} mono /> : null}
            {unreadable ? (
              <p className="text-sm text-muted-foreground">{t("https.profile.unreadable")}</p>
            ) : null}
            <FieldDescription id="acme-profile-helper">
              {t("https.profile.helper")}
            </FieldDescription>
          </Field>

          {failed ? (
            <Alert variant="destructive">
              <AlertDescription>{t("common.mutationFailed")}</AlertDescription>
            </Alert>
          ) : null}

          {disabled ? null : (
            <div className="flex flex-wrap gap-2">
              <Button
                type="button"
                variant="outline"
                className={`${TOUCH_HEIGHT} max-md:w-full`}
                disabled={!changed || busy}
                onClick={submit}
              >
                {save.isPending ? (
                  <>
                    <Spinner aria-hidden="true" />
                    {t("https.profile.saving")}
                  </>
                ) : (
                  t("https.profile.save")
                )}
              </Button>
            </div>
          )}
        </CollapsibleContent>
      </Collapsible>
    </section>
  );
}
