// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { type ProblemField, readProblem } from "@/api/problem";
import { LIST_REFETCH_MS } from "@/api/resources";
import type { components } from "@/api/schema";

export type AddressSpace = components["schemas"]["AddressSpaceBody"];
export type AddressPool = components["schemas"]["AddressPoolBody"];
export type OverlayUsage = components["schemas"]["OverlayUsageBody"];
export type OrphanRow = components["schemas"]["OrphanRowBody"];
export type OrphansWithheld = components["schemas"]["OrphansWithheldBody"];
export type KeptRow = components["schemas"]["KeptRowBody"];
export type OrphanDelete = components["schemas"]["OrphanDeleteBody"];
export type KeepTarget = components["schemas"]["KeepTarget"];
export type SubnetCheckRequest = components["schemas"]["SubnetCheckRequest"];
export type SubnetCheck = components["schemas"]["SubnetCheckBody"];
export type SubnetFinding = components["schemas"]["SubnetFindingBody"];
export type RouteCheck = components["schemas"]["RouteCheckBody"];
export type AddressSettings = components["schemas"]["AddressSettingsBody"];
export type PoolEntry = components["schemas"]["PoolEntryBody"];

// The key of the environment's address space, so the events feed can make it stale.
export function addressSpaceKey(envId: string) {
  return ["env", envId, "address-space"] as const;
}

// The environment's pools, its ingress and its overlays against their capacity. Docker is the
// source of truth, so nothing here is kept for another visit: the page reads again on a network,
// container or service event and every 30 s while the tab is visible (TanStack does not poll a
// hidden tab). The rows of another environment are never carried over.
export function addressSpaceQuery(envId: string) {
  return queryOptions({
    queryKey: addressSpaceKey(envId),
    queryFn: async (): Promise<AddressSpace> => {
      const { data } = await api.GET("/api/env/{envId}/address-space", {
        params: { path: { envId } },
      });
      if (!data) throw new Error("GET address-space failed");
      return data;
    },
    refetchInterval: LIST_REFETCH_MS,
  });
}

// OrphanRequestError is a delete, keep or stop-keeping the server refused or never answered. The
// status is 0 when the server could not be reached; the type is the problem type it named
// (inspect_too_large, network_in_use, network_builtin, network_not_found, kept_by_label,
// keep_not_found, node_required ...).
export class OrphanRequestError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string, method = "DELETE") {
    super(`${method} address-space failed (${status})`);
    this.name = "OrphanRequestError";
    this.status = status;
    this.type = type;
  }
}

// deleteOrphanNetwork deletes one orphaned or unused network. The server decides again, from a
// fresh read, whether it may: a network that is in use again, or that it cannot be sure of, is
// answered "kept", which is a safe result and not an error. It never retries (P2 D-42): an
// answer that was lost may mean the network is gone, and the next read says which.
export async function deleteOrphanNetwork(
  envId: string,
  networkId: string,
  nodeId?: string,
): Promise<OrphanDelete> {
  try {
    const res = await api.DELETE("/api/env/{envId}/address-space/orphans/{networkId}", {
      params: {
        path: { envId, networkId },
        ...(nodeId ? { query: { nodeId } } : {}),
      },
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new OrphanRequestError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status.
    throw e instanceof OrphanRequestError ? e : new OrphanRequestError(0);
  }
}

// useKeepNetworks is the mutation behind Keep: the address space is read again whatever the
// answer. It does not retry.
export function useKeepNetworks(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<number, OrphanRequestError, KeepTarget[]>({
    retry: false,
    mutationFn: async (targets) => {
      try {
        const res = await api.PUT("/api/env/{envId}/address-space/keeps", {
          params: { path: { envId } },
          body: { targets },
        });
        if (!res.data) {
          const problem = readProblem(res.error, res.response);
          throw new OrphanRequestError(problem.status, problem.type, "PUT");
        }
        return res.data.kept;
      } catch (e) {
        throw e instanceof OrphanRequestError ? e : new OrphanRequestError(0, undefined, "PUT");
      }
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) }),
  });
}

// useUnkeepNetwork is the mutation behind Stop keeping: a keep is found by the network's name and,
// for a local network, its node. It does not retry.
export function useUnkeepNetwork(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<void, OrphanRequestError, { name: string; nodeId?: string }>({
    retry: false,
    mutationFn: async ({ name, nodeId }) => {
      try {
        const res = await api.DELETE("/api/env/{envId}/address-space/keeps/{name}", {
          params: { path: { envId, name }, ...(nodeId ? { query: { nodeId } } : {}) },
        });
        if (!res.response.ok) {
          const problem = readProblem(res.error, res.response);
          throw new OrphanRequestError(problem.status, problem.type);
        }
      } catch (e) {
        throw e instanceof OrphanRequestError ? e : new OrphanRequestError(0);
      }
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) }),
  });
}

// SubnetCheckError is a subnet check the server refused or never answered. The status is 0 when
// the server could not be reached; 429 is the per-user limit (rate_limited).
export class SubnetCheckError extends Error {
  readonly status: number;
  readonly type?: string;
  constructor(status: number, type?: string) {
    super(`POST subnet-check failed (${status})`);
    this.name = "SubnetCheckError";
    this.status = status;
    this.type = type;
  }
}

// checkSubnet asks the server what a typed subnet collides with and, when asked, for the lowest
// free block of a size and the size a number of tasks needs. It only reads and reserves nothing, so
// it may be repeated; the signal lets a newer question cancel a stale one.
export async function checkSubnet(
  envId: string,
  body: SubnetCheckRequest,
  signal?: AbortSignal,
): Promise<SubnetCheck> {
  try {
    const res = await api.POST("/api/env/{envId}/address-space/subnet-check", {
      params: { path: { envId } },
      body,
      signal,
    });
    if (!res.data) {
      const problem = readProblem(res.error, res.response);
      throw new SubnetCheckError(problem.status, problem.type);
    }
    return res.data;
  } catch (e) {
    // A network failure has no status. A cancelled request is the caller's to ignore.
    if (e instanceof DOMException && e.name === "AbortError") throw e;
    throw e instanceof SubnetCheckError ? e : new SubnetCheckError(0);
  }
}

// AddressSettingsError is a read or save of the environment's address settings the server refused
// or never answered. The status is 0 when the server could not be reached; the type is the problem
// type it named (settings_changed, not_standalone, pool_base_invalid, pool_size_invalid,
// pool_overlap ...) and the fields say which row of the pools a 422 is about.
export class AddressSettingsError extends Error {
  readonly status: number;
  readonly type?: string;
  readonly fields: ProblemField[];
  constructor(status: number, type?: string, fields: ProblemField[] = []) {
    super(`address settings request failed (${status})`);
    this.name = "AddressSettingsError";
    this.status = status;
    this.type = type;
    this.fields = fields;
  }
}

// conflictBy is who saved the settings last, as a 409 settings_changed names them. Empty when the
// server did not know the person.
export function conflictBy(error: AddressSettingsError): string {
  return error.fields.find((f) => f.location === "changedBy")?.message ?? "";
}

// poolRowError is the pool row a 422 names ("body.pools[1].base" is row 1, counted from 0) and the
// problem type, so the row can show the line.
export function poolRowError(error: AddressSettingsError): { row: number; type: string } | null {
  if (!error.type) return null;
  for (const f of error.fields) {
    const m = /^body\.pools\[(\d+)\]/.exec(f.location);
    if (m) return { row: Number(m[1]), type: error.type };
  }
  return null;
}

export function addressSettingsKey(envId: string) {
  return ["env", envId, "address-settings"] as const;
}

async function settingsFailure(error: unknown, response: { status: number }): Promise<never> {
  const problem = readProblem(error, response);
  throw new AddressSettingsError(problem.status, problem.type, problem.fields);
}

// The environment's small-subnet opt-in and entered pools, with the version a save must send back.
// Docker is not asked: these are TierPlan's own settings.
export function addressSettingsQuery(envId: string) {
  return queryOptions({
    queryKey: addressSettingsKey(envId),
    queryFn: async (): Promise<AddressSettings> => {
      try {
        const res = await api.GET("/api/env/{envId}/address-space/settings", {
          params: { path: { envId } },
        });
        if (!res.data) return await settingsFailure(res.error, res.response);
        return res.data;
      } catch (e) {
        throw e instanceof AddressSettingsError ? e : new AddressSettingsError(0);
      }
    },
  });
}

export interface SmallSubnetsSave {
  enabled: boolean;
  mask: number;
  // The version the person read the settings at.
  version: number;
}

// useSaveSmallSubnets is the mutation behind Save subnet settings. It sends the version read, so a
// save made after someone else's is refused (409 settings_changed). It does not retry. A saved
// answer replaces the settings; the address space is read again because the pools' meaning does not
// change but the review of a stack does.
export function useSaveSmallSubnets(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<AddressSettings, AddressSettingsError, SmallSubnetsSave>({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/env/{envId}/address-space/small-subnets", {
          params: { path: { envId } },
          body,
        });
        if (!res.data) return await settingsFailure(res.error, res.response);
        return res.data;
      } catch (e) {
        throw e instanceof AddressSettingsError ? e : new AddressSettingsError(0);
      }
    },
    onSuccess: (saved) => {
      queryClient.setQueryData(addressSettingsKey(envId), saved);
      void queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) });
    },
  });
}

export interface AddressPoolsSave {
  pools: PoolEntry[];
  version: number;
}

// useSaveAddressPools is the mutation behind Save address pools. Like the subnet settings it sends
// the version read and does not retry. The address space is read again, because its pools now come
// from what was entered.
export function useSaveAddressPools(envId: string) {
  const queryClient = useQueryClient();
  return useMutation<AddressSettings, AddressSettingsError, AddressPoolsSave>({
    retry: false,
    mutationFn: async (body) => {
      try {
        const res = await api.PUT("/api/env/{envId}/address-space/pools", {
          params: { path: { envId } },
          body,
        });
        if (!res.data) return await settingsFailure(res.error, res.response);
        return res.data;
      } catch (e) {
        throw e instanceof AddressSettingsError ? e : new AddressSettingsError(0);
      }
    },
    onSuccess: (saved) => {
      queryClient.setQueryData(addressSettingsKey(envId), saved);
      void queryClient.invalidateQueries({ queryKey: addressSpaceKey(envId) });
    },
  });
}
