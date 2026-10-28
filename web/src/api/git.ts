// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import type { components } from "@/api/schema";
import { invalidateStacks } from "@/api/stacks";

export type GitRepo = components["schemas"]["GitRepoBody"];
export type GitCredential = components["schemas"]["GitCredentialBody"];
export type GitRepoRequest = components["schemas"]["GitRepoRequest"];
export type GitCredentialRequest = components["schemas"]["GitCredentialRequest"];
export type GitTestRequest = components["schemas"]["GitTestRequest"];
export type GitTestResult = components["schemas"]["GitTestResultBody"];
export type GitTestStepState = components["schemas"]["GitTestStepBody"]["state"];
export type GitTestFailureCode = components["schemas"]["GitTestFailureBody"]["code"];
export type GitCredentialType = GitCredential["type"];

// How often the Last test and Last use cells are read again while the page is open. There is
// no stream for git connections (UI-SPEC "Live data"): the tables refetch on their own
// mutations, plus this 30 s fallback.
export const GIT_REFETCH_MS = 30_000;

// Every git connection query sits under this key, so one invalidation refreshes both lists and
// whatever a sheet shows. No key ever holds a typed secret.
export const GIT_KEY = ["git"] as const;

// The ["git", "repos"] query: every repository the caller may see, instance ones first. The
// server answers one list for both layers.
export const gitReposQuery = queryOptions({
  queryKey: [...GIT_KEY, "repos"],
  queryFn: async () => {
    const { data } = await api.GET("/api/git/repos");
    if (!data) throw new Error("GET /api/git/repos failed");
    return data.items ?? [];
  },
  refetchInterval: GIT_REFETCH_MS,
  refetchOnWindowFocus: true,
});

// The ["git", "credentials"] query: every credential the caller may see. The server never
// includes a secret: an https token shows as its last characters, an ssh key as its public key.
export const gitCredentialsQuery = queryOptions({
  queryKey: [...GIT_KEY, "credentials"],
  queryFn: async () => {
    const { data } = await api.GET("/api/git/credentials");
    if (!data) throw new Error("GET /api/git/credentials failed");
    return data.items ?? [];
  },
  refetchInterval: GIT_REFETCH_MS,
  refetchOnWindowFocus: true,
});

// GitRequestError is a request the server refused or that got no answer. status is 0 when the
// server could not be reached; type is the problem type it named (name_taken,
// credential_in_use, validation_failed, rate_limited ...); fields holds the codes of a 422,
// one per field at fault. The submitted body, which may hold a secret, is never copied in.
export class GitRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  // How many repositories use a credential a delete was refused for (409 credential_in_use).
  get usedBy(): number | undefined {
    const n = Number(this.fields.find((f) => f.location === "conflict.count")?.message);
    return Number.isFinite(n) ? n : undefined;
  }
  constructor(status: number, type?: string, fields: ProblemField[] = [], method = "GET") {
    super(`${method} git failed (${status})`);
    this.name = "GitRequestError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }
}

interface Answer<T> {
  data?: T;
  error?: unknown;
  response: { status: number; ok: boolean };
}

// run unwraps an answer of the typed client: the data, or a GitRequestError built from the
// problem. A thrown network error becomes status 0.
async function run<T>(method: string, call: () => Promise<Answer<T>>): Promise<T> {
  try {
    const res = await call();
    if (res.data === undefined) {
      const problem = readProblem(res.error, res.response);
      throw new GitRequestError(problem.status, problem.type, problem.fields, method);
    }
    return res.data;
  } catch (e) {
    throw e instanceof GitRequestError ? e : new GitRequestError(0, undefined, [], method);
  }
}

// runVoid is run for an answer with no body (a delete).
async function runVoid(method: string, call: () => Promise<Answer<unknown>>): Promise<void> {
  try {
    const res = await call();
    if (!res.response.ok) {
      const problem = readProblem(res.error, res.response);
      throw new GitRequestError(problem.status, problem.type, problem.fields, method);
    }
  } catch (e) {
    throw e instanceof GitRequestError ? e : new GitRequestError(0, undefined, [], method);
  }
}

// What a create is given: the request and the environment of its layer (absent for the instance).
export interface CreateVariables<B> {
  body: B;
  envId?: string;
}

// What an update is given: the row, the request and the environment of its layer.
export interface UpdateVariables<B> {
  id: string;
  body: B;
  envId?: string;
}

// What a delete is given: the row and the environment of its layer.
export interface DeleteVariables {
  id: string;
  envId?: string;
}

// Every write below never retries (P2 D-42: a create whose answer was lost may have happened),
// reads both lists again whatever the answer, and has no cache time for its variables, which
// may hold a typed token or private key.
function useGitMutation<R, V>(
  fn: (variables: V) => Promise<R>,
  invalidate: boolean = true,
  gc = true,
) {
  const queryClient = useQueryClient();
  return useMutation<R, GitRequestError, V>({
    mutationFn: fn,
    retry: false,
    ...(gc ? { gcTime: 0 } : {}),
    onSettled: () => {
      if (invalidate) void queryClient.invalidateQueries({ queryKey: GIT_KEY });
    },
  });
}

export function useCreateGitRepo() {
  return useGitMutation<GitRepo, CreateVariables<GitRepoRequest>>(({ body, envId }) =>
    run("POST", () =>
      envId
        ? api.POST("/api/env/{envId}/git/repos", { params: { path: { envId } }, body })
        : api.POST("/api/git/repos", { body }),
    ),
  );
}

export function useUpdateGitRepo() {
  return useGitMutation<GitRepo, UpdateVariables<GitRepoRequest>>(({ id, body, envId }) =>
    run("PUT", () =>
      envId
        ? api.PUT("/api/env/{envId}/git/repos/{repoId}", {
            params: { path: { envId, repoId: id } },
            body,
          })
        : api.PUT("/api/git/repos/{repoId}", { params: { path: { repoId: id } }, body }),
    ),
  );
}

export function useDeleteGitRepo() {
  return useGitMutation<void, DeleteVariables>(
    ({ id, envId }) =>
      runVoid("DELETE", () =>
        envId
          ? api.DELETE("/api/env/{envId}/git/repos/{repoId}", {
              params: { path: { envId, repoId: id } },
            })
          : api.DELETE("/api/git/repos/{repoId}", { params: { path: { repoId: id } } }),
      ),
    true,
    false,
  );
}

export function useCreateGitCredential() {
  return useGitMutation<GitCredential, CreateVariables<GitCredentialRequest>>(({ body, envId }) =>
    run("POST", () =>
      envId
        ? api.POST("/api/env/{envId}/git/credentials", { params: { path: { envId } }, body })
        : api.POST("/api/git/credentials", { body }),
    ),
  );
}

export function useUpdateGitCredential() {
  return useGitMutation<GitCredential, UpdateVariables<GitCredentialRequest>>(
    ({ id, body, envId }) =>
      run("PUT", () =>
        envId
          ? api.PUT("/api/env/{envId}/git/credentials/{credentialId}", {
              params: { path: { envId, credentialId: id } },
              body,
            })
          : api.PUT("/api/git/credentials/{credentialId}", {
              params: { path: { credentialId: id } },
              body,
            }),
      ),
  );
}

export function useDeleteGitCredential() {
  return useGitMutation<void, DeleteVariables>(
    ({ id, envId }) =>
      runVoid("DELETE", () =>
        envId
          ? api.DELETE("/api/env/{envId}/git/credentials/{credentialId}", {
              params: { path: { envId, credentialId: id } },
            })
          : api.DELETE("/api/git/credentials/{credentialId}", {
              params: { path: { credentialId: id } },
            }),
      ),
    true,
    false,
  );
}

// What testing an unsaved address is given: the address, the credential to sign in with and the
// environment of the layer (absent for the instance family).
export interface TestAddressVariables {
  body: GitTestRequest;
  envId?: string;
}

// useTestGitAddress is the mutation behind the sheet's Test repository: it works on the values of
// the form, saved or not, and writes nothing unless it signed in with a credential (the server
// audits that). Being a mutation, a call replaces the one before it, so only the latest answer is
// shown. A test that signed in changes the credential's Last use, so the lists are read again.
export function useTestGitAddress() {
  const queryClient = useQueryClient();
  return useMutation<GitTestResult, GitRequestError, TestAddressVariables>({
    mutationFn: ({ body, envId }) =>
      run("POST", () =>
        envId
          ? api.POST("/api/env/{envId}/git/repos/test", { params: { path: { envId } }, body })
          : api.POST("/api/git/repos/test", { body }),
      ),
    retry: false,
    gcTime: 0,
    onSuccess: (_result, { body }) => {
      if (body.credentialId) void queryClient.invalidateQueries({ queryKey: GIT_KEY });
    },
  });
}

// useTestGitRepo tests a saved repository (the row action Test); the server records the outcome
// as the repository's last test, so the lists are read again whatever the answer.
export function useTestGitRepo() {
  return useGitMutation<GitTestResult, DeleteVariables>(
    ({ id, envId }) =>
      run("POST", () =>
        envId
          ? api.POST("/api/env/{envId}/git/repos/{repoId}/test", {
              params: { path: { envId, repoId: id } },
            })
          : api.POST("/api/git/repos/{repoId}/test", { params: { path: { repoId: id } } }),
      ),
    true,
    false,
  );
}

// What trusting a host key is given: the repository, its layer's environment (absent for the
// instance), and the key exactly as a test showed it (hostKey.key of an untrusted or changed
// answer), after the person compared its fingerprint.
export interface TrustHostKeyVariables {
  id: string;
  envId?: string;
  hostKey: string;
}

// useTrustGitHostKey stores the key a person confirmed as the one the repository's host is trusted
// by, replacing the earlier one. It is the only way a changed key is accepted, and the server
// records who did it. The lists are read again whatever the answer: the repository's Host key and
// Last test change.
export function useTrustGitHostKey() {
  return useGitMutation<GitRepo, TrustHostKeyVariables>(({ id, envId, hostKey }) =>
    run("POST", () =>
      envId
        ? api.POST("/api/env/{envId}/git/repos/{repoId}/trust-host-key", {
            params: { path: { envId, repoId: id } },
            body: { hostKey },
          })
        : api.POST("/api/git/repos/{repoId}/trust-host-key", {
            params: { path: { repoId: id } },
            body: { hostKey },
          }),
    ),
  );
}

export type GitRefs = components["schemas"]["GitRefsBody"];
export type GitRef = components["schemas"]["GitRefBody"];
export type GitTree = components["schemas"]["GitTreeBody"];
export type GitTreeEntry = components["schemas"]["GitTreeEntryBody"];
export type GitRefKind = components["schemas"]["GitStackCreateRequest"]["refKind"];
export type GitStackCreateRequest = components["schemas"]["GitStackCreateRequest"];
export type GitStackCreated = components["schemas"]["GitStackCreateBody"];
export type GitStackCheckRequest = components["schemas"]["GitStackCheckRequest"];
export type GitStackCheck = components["schemas"]["GitStackCheckBody"];

// The ["git", "env", envId, "repos"] query: the repositories a stack of this environment can use,
// its own and the instance's. The From git page offers these and no other.
export function gitEnvReposQuery(envId: string) {
  return queryOptions({
    queryKey: [...GIT_KEY, "env", envId, "repos"],
    queryFn: async () => {
      const list = await run("GET", () =>
        api.GET("/api/env/{envId}/git/repos", { params: { path: { envId } } }),
      );
      return list.items ?? [];
    },
    refetchOnWindowFocus: true,
  });
}

// The ["git", "refs", ...] query: every branch and tag of one repository, read from the git host.
// It is a read that talks to another machine, so it is not read again on its own and a failure is
// not retried: the page says why and offers Retry.
export function gitRefsQuery(envId: string, repoId: string) {
  return queryOptions({
    queryKey: [...GIT_KEY, "refs", envId, repoId],
    queryFn: async () =>
      run("GET", () =>
        api.GET("/api/env/{envId}/git/repos/{repoId}/refs", {
          params: { path: { envId, repoId } },
        }),
      ),
    retry: false,
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });
}

// The ["git", "tree", ...] query: the entries of one folder of a repository at a branch, tag or
// commit. The key holds the ref and the folder, so each folder is its own entry and stepping back
// up shows the earlier one at once.
export function gitTreeQuery(
  envId: string,
  repoId: string,
  refKind: GitRefKind,
  ref: string,
  path: string,
) {
  return queryOptions({
    queryKey: [...GIT_KEY, "tree", envId, repoId, refKind, ref, path],
    queryFn: async () =>
      run("GET", () =>
        api.GET("/api/env/{envId}/git/repos/{repoId}/tree", {
          params: { path: { envId, repoId }, query: { refKind, ref, ...(path ? { path } : {}) } },
        }),
      ),
    retry: false,
    staleTime: 30_000,
    refetchOnWindowFocus: false,
  });
}

// useCreateGitStack makes a stack from a folder of a repository. Nothing is deployed. It is never
// retried (a create whose answer was lost may have happened) and reads the stack lists again
// whatever the answer.
export function useCreateGitStack(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<GitStackCreated, GitRequestError, GitStackCreateRequest>({
    mutationFn: (body) =>
      run("POST", () =>
        api.POST("/api/env/{envId}/git-stacks", { params: { path: { envId } }, body }),
      ),
    retry: false,
    gcTime: 0,
    onSettled: () => invalidateStacks(queryClient, envId),
  });
}

// useCheckGitStackSource asks what a folder and its compose files would give, saving nothing.
// The create page's check summary (09-16) is built on it.
export function useCheckGitStackSource(envId: string) {
  return useMutation<GitStackCheck, GitRequestError, GitStackCheckRequest>({
    mutationFn: (body) =>
      run("POST", () =>
        api.POST("/api/env/{envId}/git-stacks/check", { params: { path: { envId } }, body }),
      ),
    retry: false,
    gcTime: 0,
  });
}
