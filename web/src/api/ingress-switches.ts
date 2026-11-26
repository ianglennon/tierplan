// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { IngressActionError, IngressLoadError } from "@/api/ingress";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type IngressJob = components["schemas"]["IngressJobBody"];
export type IngressFinding = components["schemas"]["IngressFindingBody"];
export type IngressRedeploy = components["schemas"]["IngressRedeployBody"];
export type IngressVIPPreviewManager = components["schemas"]["IngressVIPPreviewManagerBody"];

// How often a running job is read (UI-SPEC S6, D-24): no stream, a read every 2 seconds.
export const INGRESS_JOB_REFETCH_MS = 2_000;

// ReviewPreview is what every switch's preview answers, in the one shape the review dialog draws: the findings (an
// error refuses the switch, a warning lets it go on), the stacks that must be redeployed to join the ingress network,
// and the nodes whose agent is not connected now.
//
// The virtual IP's preview adds three things the others do not have: the fixed privileged-services warning (always
// sent, drawn first), each manager's opt-in, and for a change what it does to the address. All three are drawn as sent.
export interface ReviewPreview {
  findings: IngressFinding[];
  redeploy: IngressRedeploy[];
  unreachable: string[];
  warning?: string;
  managers?: IngressVIPPreviewManager[];
  notice?: string;
}

// VIPInput is what the virtual IP form asks for: the address with its prefix length, and a router ID or none to let
// TierPlan derive one from the swarm's cluster ID.
export interface VIPInput {
  address: string;
  routerId?: number;
}

function vipBody(input: VIPInput): { address: string; routerId?: number } {
  return {
    address: input.address.trim(),
    ...(input.routerId !== undefined ? { routerId: input.routerId } : {}),
  };
}

// IngressStarted is a switch accepted: the job to follow.
export interface IngressStarted {
  jobId: string;
}

// refused is a write the server answered with a problem: its status, its own sentence and its type, as sent.
function refused(error: unknown, response: Response): IngressActionError {
  const problem = readProblem(error, response);
  return new IngressActionError(problem.status, problem.detail, problem.type);
}

// useIngressEnablePreview reads what switching ingress on would do (D-07). It is a POST that writes nothing, run as a
// mutation so that it runs when the dialog opens and the answer is dropped the moment it is reset (gcTime 0): a
// preview is only true at the moment it is read, and the switch checks everything again.
export function useIngressEnablePreview(envId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<ReviewPreview> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/enable/preview", {
        params: { path: { envId } },
        body: {},
      });
      if (!data) throw refused(error, response);
      return {
        findings: data.findings ?? [],
        redeploy: data.redeploy ?? [],
        unreachable: [],
      };
    },
  });
}

// useEnableIngress switches ingress on. The server checks the preflight again and answers 202 with the job's ID, or 409
// with its own sentence: operation_locked names who holds the environment.
export function useEnableIngress(envId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<IngressStarted> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/enable", {
        params: { path: { envId } },
        body: {},
      });
      if (!data) throw refused(error, response);
      return { jobId: data.jobId };
    },
  });
}

// useIngressDisablePreview reads what switching ingress off would remove and what stands in the way.
export function useIngressDisablePreview(envId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<ReviewPreview> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/disable/preview", {
        params: { path: { envId } },
      });
      if (!data) throw refused(error, response);
      return {
        findings: data.findings ?? [],
        redeploy: [],
        unreachable: data.unreachable ?? [],
      };
    },
  });
}

// useDisableIngress switches ingress off, as a job like the switch on.
export function useDisableIngress(envId: string) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<IngressStarted> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/disable", {
        params: { path: { envId } },
      });
      if (!data) throw refused(error, response);
      return { jobId: data.jobId };
    },
  });
}

// useVIPPreview reads what switching the virtual IP on, or changing it, would do (D-05, D-07). The server decides which
// from whether the environment has one, and says so in the answer. The input is read when the check runs, so the form
// that owns it passes what it holds now. A refusal that is a finding (vip_outside_node_subnet and the like) comes back
// as a 200 with a finding of severity error; an address that is not an address is a 422 thrown as IngressActionError.
export function useVIPPreview(envId: string, input: VIPInput) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<ReviewPreview> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/vip/preview", {
        params: { path: { envId } },
        body: vipBody(input),
      });
      if (!data) throw refused(error, response);
      return {
        findings: data.findings ?? [],
        redeploy: [],
        unreachable: [],
        warning: data.warning,
        managers: data.managers ?? [],
        notice: data.notice,
      };
    },
  });
}

// useEnableVIP switches the virtual IP on. The server checks everything again and answers 202 with the job's ID, or
// refuses with the finding's code as the type and its sentence as the detail.
export function useEnableVIP(envId: string, input: VIPInput) {
  return useMutation({
    gcTime: 0,
    mutationFn: async (): Promise<IngressStarted> => {
      const { data, error, response } = await api.POST("/api/env/{envId}/ingress/vip", {
        params: { path: { envId } },
        body: vipBody(input),
      });
      if (!data) throw refused(error, response);
      return { jobId: data.jobId };
    },
  });
}

// jobEnded says whether a job has finished, one way or another.
export function jobEnded(job: IngressJob | undefined): boolean {
  return job !== undefined && job.state !== "running";
}

// ingressJobQuery reads one ingress job and its steps, again every 2 s while it runs and not at all once it ended. A
// job the server will not show (403, 404: only its starter and an Admin may read it, A-02) is not read again either;
// any other failure is a read that missed and the next one tries again.
export function ingressJobQuery(envId: string, jobId: string) {
  return queryOptions({
    // Not under the page's own key: a refresh of the page after the switch must not read the ended job again.
    queryKey: ["env", envId, "ingress-job", jobId] as const,
    queryFn: async (): Promise<IngressJob> => {
      const res = await api.GET("/api/env/{envId}/ingress/jobs/{jobId}", {
        params: { path: { envId, jobId } },
      });
      if (!res.data) {
        const problem = readProblem(res.error, res.response);
        throw new IngressLoadError(problem.status, problem.type);
      }
      return res.data;
    },
    retry: false,
    gcTime: 0,
    refetchInterval: (query) => {
      if (jobEnded(query.state.data)) return false;
      const error = query.state.error;
      if (error instanceof IngressLoadError && (error.status === 403 || error.status === 404)) {
        return false;
      }
      return INGRESS_JOB_REFETCH_MS;
    },
  });
}
