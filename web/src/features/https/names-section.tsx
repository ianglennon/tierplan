// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Link } from "@tanstack/react-router";

import type { HTTPSACME } from "@/api/https";
import { Sentence, SLOT } from "@/features/https/sentence";
import { useT } from "@/i18n/use-t";

// NamesSection shows the exact names a certificate would carry, before any order (ACME-02). A name
// Let's Encrypt cannot use stays listed and is marked "Not allowed"; the reason is in the blocked
// list. When the stored certificate covers other names, the two sets are shown side by side as
// words, so the difference is not left to colour. Every name is text from a setting or a
// certificate and is shown as text only.
export function NamesSection({ acme }: { acme: HTTPSACME }) {
  const t = useT();
  const names = acme.names ?? [];
  const link = (
    <Link to="/settings/general" className="text-accent-ink underline underline-offset-2">
      {t("https.names.link")}
    </Link>
  );
  const mismatch = acme.namesMismatch && acme.certificate !== undefined;

  return (
    <section className="flex flex-col gap-2" aria-labelledby="https-names-title">
      <h4 id="https-names-title" className="text-sm font-semibold">
        {t("https.names.title")}
      </h4>
      {mismatch ? (
        <div className="flex flex-col gap-3">
          <NameList
            label={t("https.names.onCertificate")}
            names={(acme.certificate?.names ?? []).map((name) => ({ name, refused: false }))}
          />
          <NameList label={t("https.names.neededNow")} names={names} />
        </div>
      ) : names.length > 0 ? (
        <NameList names={names} />
      ) : (
        <p className="text-sm text-muted-foreground">
          <Sentence text={t("https.names.none", { link: SLOT })} slot={link} />
        </p>
      )}
      <p className="text-sm text-muted-foreground">
        <Sentence text={t("https.names.helper", { link: SLOT })} slot={link} />
      </p>
    </section>
  );
}

function NameList({
  label,
  names,
}: {
  label?: string;
  names: { name: string; refused: boolean }[];
}) {
  const t = useT();
  return (
    <div className="flex flex-col gap-1">
      {label ? <p className="text-sm font-medium">{label}</p> : null}
      <ul className="flex flex-col gap-2" aria-label={label}>
        {names.map(({ name, refused }) => (
          <li key={name} className="min-w-0 break-all text-sm">
            <span className="font-mono">{name}</span>
            {refused ? (
              <span className="ml-2 font-medium text-destructive">
                {t("https.names.notAllowed")}
              </span>
            ) : null}
          </li>
        ))}
      </ul>
    </div>
  );
}
