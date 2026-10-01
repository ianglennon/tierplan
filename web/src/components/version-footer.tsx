// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useEffect, useState } from "react";

import { api } from "@/api/client";

interface Version {
  display: string;
  sourceUrl: string;
  sourceTreeUrl: string;
}

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

  if (failed) return <footer>Version unavailable</footer>;
  if (!version) return <footer aria-busy="true">&nbsp;</footer>;

  return (
    <footer>
      <a href={version.sourceTreeUrl} target="_blank" rel="noopener noreferrer">
        Bayplan {version.display}
      </a>
      {" · "}
      <a
        href={version.sourceUrl}
        target="_blank"
        rel="noopener noreferrer"
        aria-label="Source code (opens in a new tab)"
      >
        Source code
      </a>
    </footer>
  );
}
