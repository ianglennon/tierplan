// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { Link } from "@tanstack/react-router";
import { useState } from "react";

import { NoPermission } from "@/components/full-page-state";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "@/components/ui/empty";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { MAX_QUERY } from "@/features/containers/list-search";
import { PullDialog, usePullEnvironments } from "@/features/pulls/pull-dialog";
import { TagBrowser, type TagFilterPatch } from "@/features/tags/tag-browser";
import { useAnyEnvironmentPermission, useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { z } from "@/lib/zod-config";

// The longest value a search parameter holds that is an ID.
const MAX_ID = 64;
// The server's own limit on a reference.
const MAX_REF = 512;

// The route's search (UI-SPEC "Screens"): the repository, which is required, and the
// environment whose credentials count as well. A value that does not fit is dropped, so a
// stale or hand-edited link shows the empty page and not an error.
export const browseSearchSchema = z.object({
  ref: z.string().max(MAX_REF).catch(""),
  q: z.string().min(1).max(MAX_QUERY).optional().catch(undefined),
  // hide=0 shows the signature, attestation and cache tags; they are hidden otherwise.
  hide: z.literal(0).optional().catch(undefined),
  envId: z.string().min(1).max(MAX_ID).optional().catch(undefined),
});

export type BrowseSearch = z.output<typeof browseSearchSchema>;

// A change to the search: a key set to undefined leaves the address.
export type BrowseSearchPatch = { [K in keyof BrowseSearch]?: BrowseSearch[K] | undefined };

function Breadcrumb() {
  const t = useT();
  return (
    <nav aria-label={t("tags.page.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link to="/registries" className="rounded-sm text-accent-ink hover:underline">
            {t("tags.page.breadcrumb.registries")}
          </Link>
        </li>
        <li>
          <span aria-current="page">{t("tags.page.breadcrumb.browse")}</span>
        </li>
      </ol>
    </nav>
  );
}

// BrowsePage is /registries/browse: the breadcrumb, the repository as the title, and the
// tag browser with the credential line (D-11). It is for anyone who reads images on some
// environment; the server limits the credentials to the ones they may use (D-24a).
export function BrowsePage({
  search,
  onSearchChange,
}: {
  search: BrowseSearch;
  onSearchChange?: (patch: BrowseSearchPatch) => void;
}) {
  const t = useT();
  const allowed = useAnyEnvironmentPermission("images.read");
  const environments = usePullEnvironments();
  // A pull asked for from a row: the form asks which environment. Kept once opened, so a pull
  // running when it is closed still ends with its toast; each new request starts a new form.
  const [pull, setPull] = useState<{ ref: string; n: number } | null>(null);
  const [pulling, setPulling] = useState(false);
  useDocumentTitle(search.ref || t("tags.page.breadcrumb.browse"));
  if (!allowed) return <NoPermission />;

  return (
    <div className="flex min-w-0 flex-col gap-4">
      <Breadcrumb />
      {search.ref ? (
        <>
          <h1 className="line-clamp-2 text-2xl font-semibold break-all">
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="font-mono">{search.ref}</span>
              </TooltipTrigger>
              <TooltipContent>{search.ref}</TooltipContent>
            </Tooltip>
          </h1>
          <TagBrowser
            repo={search.ref}
            variant="full"
            credentialEnvId={search.envId}
            onPull={(ref) => {
              setPull((prev) => ({ ref, n: (prev?.n ?? 0) + 1 }));
              setPulling(true);
            }}
            q={search.q}
            hide={search.hide !== 0}
            onFilterChange={(patch: TagFilterPatch) => {
              const next: BrowseSearchPatch = {};
              if ("q" in patch) next.q = patch.q;
              if ("hide" in patch) next.hide = patch.hide ? undefined : 0;
              onSearchChange?.(next);
            }}
          />
          {pull ? (
            <PullDialog
              key={pull.n}
              envId=""
              environments={environments}
              open={pulling}
              onOpenChange={setPulling}
              initialRef={pull.ref}
              hostName=""
              swarm={false}
            />
          ) : null}
        </>
      ) : (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>{t("tags.page.noRepository.title")}</EmptyTitle>
            <EmptyDescription>{t("tags.page.noRepository.body")}</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button asChild variant="outline">
              <Link to="/registries">{t("tags.page.noRepository.back")}</Link>
            </Button>
          </EmptyContent>
        </Empty>
      )}
    </div>
  );
}
