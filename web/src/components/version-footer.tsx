// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";

import { api } from "@/api/client";

// Build-time source URL (Vite define, env BAYPLAN_SOURCE_URL). It keeps the
// AGPL section 13 source link on screen even when /api/version is unreachable.
declare const __BAYPLAN_SOURCE_URL__: string;

// Links use the accent ink with a 2px focus ring (UI-SPEC Color, accent use 3 and 4).
const linkClass =
  "text-accent-ink underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-sm";

const footerClass = "text-xs text-muted-foreground";

async function fetchVersion() {
  const { data, error } = await api.GET("/api/version");
  if (!data) throw new Error(`GET /api/version failed: ${JSON.stringify(error)}`);
  return data;
}

// VersionFooter shows the running version, linked to the exact source tree it
// was built from, plus a link to the source root (AGPL section 13, PLAT-05).
// If the API cannot be reached it falls back to the wordmark and the
// build-time source link, so the source link is never missing.
export function VersionFooter() {
  const { t } = useTranslation();
  const { data: version, isError } = useQuery({
    queryKey: ["version"],
    queryFn: fetchVersion,
    staleTime: Number.POSITIVE_INFINITY,
  });

  if (isError) {
    return (
      <footer className={footerClass}>
        <span>{t("common.brand.wordmark")}</span>
        {" · "}
        <a
          href={__BAYPLAN_SOURCE_URL__}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={t("common.versionFooter.sourceAria")}
          className={linkClass}
        >
          {t("common.versionFooter.source")}
        </a>
      </footer>
    );
  }
  if (!version) {
    return (
      <footer className={footerClass} aria-busy="true">
        &nbsp;
      </footer>
    );
  }

  return (
    <footer className={footerClass}>
      <a
        href={version.sourceTreeUrl}
        target="_blank"
        rel="noopener noreferrer"
        className={linkClass}
      >
        {t("common.versionFooter.label", { display: version.display })}
      </a>
      {" · "}
      <a
        href={version.sourceUrl}
        target="_blank"
        rel="noopener noreferrer"
        aria-label={t("common.versionFooter.sourceAria")}
        className={linkClass}
      >
        {t("common.versionFooter.source")}
      </a>
    </footer>
  );
}
