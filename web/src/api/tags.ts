// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { infiniteQueryOptions } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type TagList = components["schemas"]["TagListBody"];
export type TagName = components["schemas"]["TagNameBody"];

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
  // An environment whose own credentials count as well as the instance's.
  envId?: string;
}

export const TAGS_KEY = ["tags"] as const;

// tagsInfiniteQuery reads a repository's tag names a page at a time. The server caches the
// names for a minute per credential, so reading again is cheap. A failure shows in place of
// the table with its own Retry, so nothing retries by itself.
export function tagsInfiniteQuery(p: TagsParams) {
  return infiniteQueryOptions({
    queryKey: [...TAGS_KEY, p.repo, p.envId ?? null],
    initialPageParam: 0,
    queryFn: async (): Promise<TagList> => {
      try {
        const res = await api.GET("/api/registries/tags", {
          params: { query: { ref: p.repo, envId: p.envId } },
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
