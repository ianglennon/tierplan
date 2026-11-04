// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { Boxes, Container, Image as ImageIcon, Layers, Network, Search } from "lucide-react";
import type { ComponentType } from "react";

import {
  SEARCH_TYPES,
  type SearchBody,
  type SearchResult,
  type SearchType,
  type SearchTypeFilter,
} from "@/api/search";
import { Button } from "@/components/ui/button";
import { CommandGroup, CommandItem } from "@/components/ui/command";
import { Spinner } from "@/components/ui/spinner";
import { MatchText } from "@/features/search/match-text";
import { useT } from "@/i18n/use-t";
import { formatBytes } from "@/lib/format";

// How many hits a type shows in the palette. The server answers with at most this many of each
// (limit=palette); the palette cuts to it as well, so a longer answer never grows the list.
export const PALETTE_PER_TYPE = 5;

// Where a hit or the results page is. The router builds the address from the route pattern and
// the params, so an ID or a name from a Docker host is encoded and never joined into a path.
export interface SearchTarget {
  to: string;
  params?: Record<string, string>;
  search?: Record<string, string | undefined>;
}

// pageTarget opens the results page. The query goes in the search part of the address, never
// in the path.
export function pageTarget(q: string, type?: SearchTypeFilter): SearchTarget {
  return { to: "/search", search: { q, type: type && type !== "all" ? type : undefined } };
}

// hitTarget is where a hit opens, in its own environment. A stack TierPlan manages is known by
// its ID; one it does not is known by its name. A container, image or network is found by its
// node as well as its ID.
export function hitTarget(hit: SearchResult): SearchTarget {
  const envId = hit.envId;
  const node = hit.nodeId || undefined;
  switch (hit.type) {
    case "stack":
      return hit.managed
        ? { to: "/env/$envId/stacks/$stackId", params: { envId, stackId: hit.id } }
        : { to: "/env/$envId/unmanaged-stacks/$name", params: { envId, name: hit.name } };
    case "service":
      return { to: "/env/$envId/services/$serviceId", params: { envId, serviceId: hit.id } };
    case "container":
      return {
        to: "/env/$envId/containers/$containerId",
        params: { envId, containerId: hit.id },
        search: { node },
      };
    case "image":
      return {
        to: "/env/$envId/images/$imageId",
        params: { envId, imageId: hit.id },
        search: { node },
      };
    case "network":
      return {
        to: "/env/$envId/networks/$networkId",
        params: { envId, networkId: hit.id },
        search: { node },
      };
  }
}

const ICONS: Record<SearchType, ComponentType<{ className?: string; "aria-hidden"?: boolean }>> = {
  stack: Boxes,
  service: Layers,
  container: Container,
  image: ImageIcon,
  network: Network,
};

const hitKey = (h: SearchResult) => `${h.type}|${h.envId}|${h.nodeId}|${h.id || h.name}`;

// detail is the muted second line of a hit. Only the name is highlighted (UI-SPEC decision 16).
function useDetail() {
  const t = useT();
  return (h: SearchResult): string => {
    switch (h.type) {
      case "stack":
        return t("search.hit.stack", { count: h.services, env: h.envName });
      case "service":
        return t("search.hit.service", { image: h.image, where: h.stack || h.envName });
      case "container":
        return t("search.hit.container", { image: h.image, env: h.envName });
      case "image":
        return t("search.hit.image", { size: formatBytes(h.size), env: h.envName });
      case "network":
        return h.subnet
          ? t("search.hit.network", { driver: h.driver, subnet: h.subnet, env: h.envName })
          : t("search.hit.networkNoSubnet", { driver: h.driver, env: h.envName });
    }
  };
}

interface SearchHitsProps {
  // What was typed, trimmed. The top row says it and opens the results page with it.
  typed: string;
  // The query the answer in body is for: the highlight follows it, which can trail the typing.
  answered: string;
  body: SearchBody | undefined;
  // True while the answer is on its way (or the typing has not paused), so "No matches" is not
  // said about a search that has not finished.
  busy: boolean;
  onOpen: (target: SearchTarget) => void;
}

// SearchHits is the palette's list while a query of 2 or more characters is typed: the top row
// first and selected (Enter opens the results page), then the five kinds in groups of at most
// five. Every name, image and environment is rendered as text.
export function SearchHits({ typed, answered, body, busy, onOpen }: SearchHitsProps) {
  const t = useT();
  const detail = useDetail();
  const results = body?.results ?? [];
  const total = body?.counts.all ?? 0;

  return (
    <>
      <CommandGroup forceMount>
        <CommandItem
          forceMount
          value="search-all"
          onSelect={() => onOpen(pageTarget(typed))}
          className="max-md:min-h-11"
        >
          <Search aria-hidden="true" />
          <span className="min-w-0 flex-1 truncate">{t("search.palette.all", { q: typed })}</span>
          <span className="text-xs text-muted-foreground" aria-hidden="true">
            {t("search.palette.enter")}
          </span>
        </CommandItem>
      </CommandGroup>
      {body !== undefined && total === 0 && !busy ? (
        <p className="px-3 py-4 text-center text-sm text-muted-foreground">
          {t("search.palette.none", { q: answered })}
        </p>
      ) : null}
      {SEARCH_TYPES.map((type) => {
        const hits = results.filter((r) => r.type === type).slice(0, PALETTE_PER_TYPE);
        if (hits.length === 0) return null;
        const count = body?.counts[type] ?? hits.length;
        const Icon = ICONS[type];
        return (
          <CommandGroup key={type} forceMount heading={t(`search.type.${type}.heading`)}>
            {hits.map((hit) => (
              <CommandItem
                key={hitKey(hit)}
                forceMount
                value={`hit|${hitKey(hit)}`}
                onSelect={() => onOpen(hitTarget(hit))}
                className="min-h-11 py-1"
              >
                <Icon className="text-muted-foreground" aria-hidden />
                <span className="flex min-w-0 flex-1 flex-col">
                  <span className="truncate font-mono text-sm" title={hit.name}>
                    <MatchText text={hit.name} q={answered} />
                  </span>
                  <span className="truncate text-xs text-muted-foreground">{detail(hit)}</span>
                </span>
              </CommandItem>
            ))}
            {count > hits.length ? (
              <CommandItem
                forceMount
                value={`all|${type}`}
                onSelect={() => onOpen(pageTarget(answered, type))}
                className="max-md:min-h-11"
              >
                <span className="min-w-0 flex-1 truncate text-muted-foreground">
                  {t("search.palette.showAll", { count, type: t(`search.type.${type}.noun`) })}
                </span>
              </CommandItem>
            ) : null}
          </CommandGroup>
        );
      })}
    </>
  );
}

interface SearchStatusProps {
  // "searching" shows the spinner and the count of environments; "done" is read out as the
  // number of results and not shown; anything else is empty. The caller passes it already
  // delayed until the typing pauses, so the region is read once and not on every keystroke.
  state: "idle" | "searching" | "done";
  environments: number;
  results: number;
}

// SearchStatus is the palette's polite live region. It is always mounted, because a region that
// appears together with its text is not reliably read.
export function SearchStatus({ state, environments, results }: SearchStatusProps) {
  const t = useT();
  return (
    <div role="status" className="px-3 text-sm text-muted-foreground">
      {state === "searching" ? (
        <div className="flex items-center gap-2 py-3">
          <Spinner role="presentation" aria-label={undefined} aria-hidden="true" />
          <span>{t("search.palette.searching", { count: environments })}</span>
        </div>
      ) : null}
      {state === "done" ? (
        <span className="sr-only">{t("search.palette.found", { count: results })}</span>
      ) : null}
    </div>
  );
}

// SearchError is the whole search failing: nothing in the list can be trusted, so it says so and
// offers the same search again. The top row stays above it.
export function SearchError({ onRetry }: { onRetry: () => void }) {
  const t = useT();
  return (
    <div role="alert" className="flex flex-col items-start gap-2 px-3 py-4 text-sm">
      <div>
        <p className="font-medium">{t("search.error.title")}</p>
        <p className="text-muted-foreground">{t("search.error.body")}</p>
      </div>
      <Button type="button" variant="outline" size="sm" onClick={onRetry}>
        {t("search.error.retry")}
      </Button>
    </div>
  );
}
