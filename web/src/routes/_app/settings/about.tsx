// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";

import { NoPermission } from "@/components/full-page-state";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { fetchVersion } from "@/components/version-footer";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// The licence is an SPDX identifier, not translatable copy.
const LICENCE = "AGPL-3.0-or-later";

// Links use the accent ink with a 2px focus ring (UI-SPEC Color, accent use 3).
const linkClass =
  "text-accent-ink underline-offset-2 hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-sm";

interface Notice {
  name: string;
  version: string;
  licence: string;
}

function isNotice(v: unknown): v is Notice {
  if (typeof v !== "object" || v === null) return false;
  const r = v as Record<string, unknown>;
  return (
    typeof r.name === "string" && typeof r.version === "string" && typeof r.licence === "string"
  );
}

// The list is a static asset written at build time by scripts/gen-notices.mjs
// and served from this origin (D-16). When the file is missing the server's SPA
// fallback answers with the app shell instead, which is not a list, so that is
// an error as well.
async function fetchNotices(): Promise<Notice[]> {
  const res = await globalThis.fetch("/third-party-notices.json");
  if (!res.ok) throw new Error(`GET /third-party-notices.json: ${res.status}`);
  const body: unknown = await res.json();
  if (!Array.isArray(body) || !body.every(isNotice)) {
    throw new Error("third-party-notices.json is not a list of notices");
  }
  return body;
}

function LoadFailed({
  message,
  retry,
  busy,
  onRetry,
}: {
  message: string;
  retry: string;
  busy: boolean;
  onRetry: () => void;
}) {
  return (
    <Alert variant="destructive">
      <AlertDescription className="flex flex-wrap items-center gap-3">
        <span>{message}</span>
        <Button type="button" variant="outline" size="sm" disabled={busy} onClick={onRetry}>
          {retry}
        </Button>
      </AlertDescription>
    </Alert>
  );
}

function Notices() {
  const t = useT();
  const notices = useQuery({
    queryKey: ["third-party-notices"],
    queryFn: fetchNotices,
    staleTime: Number.POSITIVE_INFINITY,
  });

  let body = null;
  if (notices.isPending) {
    body = (
      <div aria-busy="true">
        <span className="sr-only" role="status">
          {t("settings.about.notices.loading")}
        </span>
        <Skeleton className="h-40 w-full" />
      </div>
    );
  } else if (notices.isError) {
    body = (
      <LoadFailed
        message={t("settings.about.notices.loadFailed")}
        retry={t("settings.about.notices.retry")}
        busy={notices.isFetching}
        onRetry={() => void notices.refetch()}
      />
    );
  } else {
    body = (
      <ScrollArea
        role="region"
        aria-label={t("settings.about.notices.region")}
        className="h-80 rounded-md border"
      >
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("settings.about.notices.package")}</TableHead>
              <TableHead>{t("settings.about.notices.version")}</TableHead>
              <TableHead>{t("settings.about.notices.licence")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {notices.data.map((n) => (
              <TableRow key={`${n.name}\u0000${n.version}\u0000${n.licence}`}>
                <TableCell className="break-all whitespace-normal font-mono">{n.name}</TableCell>
                <TableCell className="font-mono">{n.version}</TableCell>
                <TableCell>{n.licence}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </ScrollArea>
    );
  }

  return (
    <section className="flex flex-col gap-3" aria-labelledby="third-party-notices-heading">
      <div className="flex flex-col gap-1">
        <h2 id="third-party-notices-heading" className="text-base font-semibold">
          {t("settings.about.notices.heading")}
        </h2>
        <p className="text-xs text-muted-foreground">{t("settings.about.notices.description")}</p>
      </div>
      {body}
    </section>
  );
}

function Build() {
  const t = useT();
  const version = useQuery({
    queryKey: ["version"],
    queryFn: fetchVersion,
    staleTime: Number.POSITIVE_INFINITY,
  });

  if (version.isPending) {
    return (
      <div aria-busy="true">
        <span className="sr-only" role="status">
          {t("settings.about.loading")}
        </span>
        <Skeleton className="h-24 w-full max-w-xl" />
      </div>
    );
  }
  if (version.isError) {
    return (
      <LoadFailed
        message={t("settings.about.loadFailed")}
        retry={t("settings.about.retry")}
        busy={version.isFetching}
        onRetry={() => void version.refetch()}
      />
    );
  }

  const v = version.data;
  return (
    <dl className="grid max-w-xl grid-cols-[8rem_1fr] gap-x-4 gap-y-2">
      <dt className="text-muted-foreground">{t("settings.about.version")}</dt>
      <dd>{v.display}</dd>
      <dt className="text-muted-foreground">{t("settings.about.commit")}</dt>
      <dd className="min-w-0">
        {v.commit ? (
          <a
            href={v.sourceTreeUrl}
            target="_blank"
            rel="noopener noreferrer"
            aria-label={t("settings.about.commitAria", { sha: v.commit })}
            className={`${linkClass} break-all font-mono`}
          >
            {v.commit}
          </a>
        ) : (
          t("settings.about.commitUnknown")
        )}
      </dd>
      <dt className="text-muted-foreground">{t("settings.about.licence")}</dt>
      <dd>{LICENCE}</dd>
      <dt className="text-muted-foreground">{t("settings.about.source")}</dt>
      <dd>
        <a
          href={v.sourceUrl}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={t("common.versionFooter.sourceAria")}
          className={linkClass}
        >
          {t("common.versionFooter.source")}
        </a>
      </dd>
    </dl>
  );
}

// Settings > About (D-60, D-61, PLAT-05): the exact build, its licence, where the
// source is and every third-party notice. Anyone with settings access can read it.
function AboutTab() {
  const t = useT();
  useDocumentTitle(t("settings.about.tabTitle"));
  const allowed = usePermission("settings.read");
  if (!allowed) return <NoPermission />;
  return (
    <div className="flex flex-col gap-8">
      <Build />
      <Notices />
    </div>
  );
}

export const Route = createFileRoute("/_app/settings/about")({
  component: AboutTab,
});
