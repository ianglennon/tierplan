// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { infiniteQueryOptions, keepPreviousData } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type TagList = components["schemas"]["TagListBody"];
export type TagName = components["schemas"]["TagNameBody"];
export type TagDetail = components["schemas"]["TagDetailBody"];
export type Platform = components["schemas"]["PlatformBody"];
export type RunningHereData = components["schemas"]["RunningHereBody"];

// How many tags one details request asks about, as the server allows.
export const TAG_DETAILS_BATCH = 25;

// The problem types a tag listing fails with, each worded by the tag browser. Anything else
// is shown as the generic failure.
export const TAG_ERROR_TYPES = [
  "needs_login",
  "login_refused",
  "repository_not_found",
  "registry_unreachable",
  "registry_tls",
  "rate_limited",
] as const;
export type TagErrorType = (typeof TAG_ERROR_TYPES)[number];

export function isTagErrorType(type: string | undefined): type is TagErrorType {
  return TAG_ERROR_TYPES.some((t) => t === type);
}

// The credential the registry was asked with when the listing failed, as the server names it.
export interface TagErrorCredential {
  id: string;
  name: string;
  // "instance", or "environment" with the environment's name.
  scopeKind: string;
  scopeName: string;
}

// TagsRequestError is a listing the server refused or that got no answer. status is 0 when
// the server could not be reached; type is the problem type it named (needs_login ...,
// ref_invalid). host and repo say which registry and repository, credential which sign-in.
export class TagsRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly host: string;
  readonly repo: string;
  readonly credential?: TagErrorCredential;
  constructor(status: number, type?: string, fields: ProblemField[] = []) {
    super(`GET registries/tags failed (${status})`);
    this.name = "TagsRequestError";
    this.status = status;
    this.type = type;
    const get = (location: string) => fields.find((f) => f.location === location)?.message;
    this.host = get("registry.host") ?? "";
    this.repo = get("registry.repo") ?? "";
    const id = get("credential.id");
    if (id) {
      this.credential = {
        id,
        name: get("credential.name") ?? "",
        scopeKind: get("credential.scopeKind") ?? "instance",
        scopeName: get("credential.scopeName") ?? "",
      };
    }
  }
}

// What a tag listing is asked for.
export interface TagsParams {
  // The repository as it was typed: ghcr.io/acme/app or nginx.
  repo: string;
  // Environment context: the tags are read through the environment's routes, with the
  // credentials the environment's pulls would use, and the rows say what runs there.
  envId?: string;
  // Registries context only: an environment whose own credentials count as well as the
  // instance's, without the page becoming that environment's.
  credentialEnvId?: string;
  // Only tags whose name contains this text.
  q?: string;
  // Leave out signature, attestation and cache tags. On unless false.
  hide?: boolean;
}

// Tags per page, as the server pages them.
export const TAGS_PAGE_SIZE = 100;

export const TAGS_KEY = ["tags"] as const;

// tagsInfiniteQuery reads a repository's tag names a page at a time. The server caches the
// names for a minute per credential, so reading again is cheap. A failure shows in place of
// the table with its own Retry, so nothing retries by itself.
export function tagsInfiniteQuery(p: TagsParams) {
  return infiniteQueryOptions({
    queryKey: [
      ...TAGS_KEY,
      p.repo,
      p.envId ?? null,
      p.credentialEnvId ?? null,
      p.q ?? null,
      p.hide !== false,
    ],
    initialPageParam: 0,
    // The list stays on screen while a new filter is read, so typing does not blank it.
    placeholderData: keepPreviousData,
    queryFn: async ({ pageParam }): Promise<TagList> => {
      try {
        // A value the server would assume anyway is left out of the address it is asked.
        const query = {
          ref: p.repo,
          q: p.q || undefined,
          hide: p.hide === false ? false : undefined,
          page: pageParam > 0 ? pageParam : undefined,
        };
        const res = p.envId
          ? await api.GET("/api/env/{envId}/images/tags", {
              params: { path: { envId: p.envId }, query },
            })
          : await api.GET("/api/registries/tags", {
              params: { query: { ...query, envId: p.credentialEnvId } },
            });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new TagsRequestError(problem.status, problem.type, problem.fields);
        }
        return res.data;
      } catch (e) {
        throw e instanceof TagsRequestError ? e : new TagsRequestError(0);
      }
    },
    getNextPageParam: (last) => (last.more ? last.page + 1 : undefined),
    retry: false,
    staleTime: 30_000,
  });
}

// What a details request is asked for: the repository as it was typed, and the environment
// whose credentials and node architecture count.
export interface TagDetailsParams {
  repo: string;
  // Environment context, as for the listing.
  envId?: string;
  credentialEnvId?: string;
}

// fetchTagDetails reads the digest, date and platforms of up to TAG_DETAILS_BATCH tags. A tag
// the registry could not read comes back with a code; only a request that failed as a whole
// throws.
export async function fetchTagDetails(p: TagDetailsParams, tags: string[]): Promise<TagDetail[]> {
  const res = p.envId
    ? await api.POST("/api/env/{envId}/images/tags/details", {
        params: { path: { envId: p.envId } },
        body: { ref: p.repo, tags },
      })
    : await api.POST("/api/registries/tags/details", {
        body: { ref: p.repo, envId: p.credentialEnvId, tags },
      });
  if (!res.data) {
    const problem = readProblem(res.error, res.response);
    throw new TagsRequestError(problem.status, problem.type, problem.fields);
  }
  return res.data.items ?? [];
}
