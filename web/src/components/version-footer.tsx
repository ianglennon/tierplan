// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useState } from "react";

import { api } from "@/api/client";

interface Version {
  display: string;
  sourceUrl: string;
  sourceTreeUrl: string;
}

// Links use the accent ink with a 2px focus ring (UI-SPEC Color, accent use 3 and 4).
const linkClass =
  "text-accent-ink underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-sm";

// VersionFooter shows the running version, linked to the exact source tree it
// was built from, plus a link to the source root (AGPL section 13, PLAT-05).
export function VersionFooter() {
  const [version, setVersion] = useState<Version | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api
      .GET("/api/version")
      .then(({ data }) => {
        if (cancelled) return;
        if (data) setVersion(data);
        else setFailed(true);
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const footerClass = "text-xs text-muted-foreground";
  if (failed) return <footer className={footerClass}>Version unavailable</footer>;
  if (!version) return <footer className={footerClass} aria-busy="true">&nbsp;</footer>;

  return (
    <footer className={footerClass}>
      <a
        href={version.sourceTreeUrl}
        target="_blank"
        rel="noopener noreferrer"
        className={linkClass}
      >
        Bayplan {version.display}
      </a>
      {" · "}
      <a
        href={version.sourceUrl}
        target="_blank"
        rel="noopener noreferrer"
        aria-label="Source code (opens in a new tab)"
        className={linkClass}
      >
        Source code
      </a>
    </footer>
  );
}
