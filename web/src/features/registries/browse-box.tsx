// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { type FormEvent, useId, useState } from "react";

import { Button } from "@/components/ui/button";
import { Field, FieldError, FieldLabel } from "@/components/ui/field";
import { Input } from "@/components/ui/input";
import { SettingsCard } from "@/features/environments/settings-card";
import { useT } from "@/i18n/use-t";

// The server's own limit on a reference.
const MAX_REF = 512;

// A repository reference as the engine reads it: an optional registry host (with a port), a
// path of lower-case segments, and an optional tag or digest. This only keeps what could never
// be one from leaving the page; the server is the judge of the rest.
const HOST =
  "(?:(?:localhost|[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)+)(?::[0-9]+)?|[A-Za-z0-9-]+:[0-9]+)/";
const SEGMENT = "[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*";
const REF = new RegExp(
  `^(?:${HOST})?${SEGMENT}(?:/${SEGMENT})*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@[A-Za-z0-9+.-]+:[A-Fa-f0-9]{32,})?$`,
);

export function looksLikeRepository(ref: string): boolean {
  return ref.length > 0 && ref.length <= MAX_REF && REF.test(ref);
}

interface BrowseBoxProps {
  // What Browse tags does with a reference that could be one: the Registries page opens
  // the tag browser for it.
  onBrowse: (ref: string) => void;
}

// BrowseBox is the "Browse a repository" card (UI-SPEC, D-11): one field and a button that
// open the tag browser for any repository. A reference that could not be one shows the field
// error and goes nowhere.
export function BrowseBox({ onBrowse }: BrowseBoxProps) {
  const t = useT();
  const id = useId();
  const [ref, setRef] = useState("");
  const [invalid, setInvalid] = useState(false);

  function submit(e: FormEvent) {
    e.preventDefault();
    const typed = ref.trim();
    if (!looksLikeRepository(typed)) {
      setInvalid(true);
      return;
    }
    onBrowse(typed);
  }

  return (
    <SettingsCard title={t("registries.browse.title")}>
      <p className="text-sm text-muted-foreground">{t("registries.browse.body")}</p>
      <form noValidate onSubmit={submit} className="flex flex-col gap-4">
        <Field data-invalid={invalid ? true : undefined}>
          <FieldLabel htmlFor={`${id}-ref`}>{t("registries.browse.repository")}</FieldLabel>
          <Input
            id={`${id}-ref`}
            value={ref}
            onChange={(e) => {
              setRef(e.target.value);
              setInvalid(false);
            }}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            placeholder={t("registries.browse.placeholder")}
            aria-invalid={invalid}
            aria-describedby={invalid ? `${id}-ref-error` : undefined}
            className="font-mono max-md:h-11"
          />
          {invalid ? (
            <FieldError id={`${id}-ref-error`}>{t("registries.browse.error")}</FieldError>
          ) : null}
        </Field>
        <div>
          <Button type="submit" variant="outline" className="max-md:h-11">
            {t("registries.browse.submit")}
          </Button>
        </div>
      </form>
    </SettingsCard>
  );
}
