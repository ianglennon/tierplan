// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { components } from "@/api/schema";
import type { Me } from "@/hooks/use-me";

// What the service detail tests share: a service as the server answers it, and the fetch stub
// that answers the page around it. The detail's masking is the server's job, so a test that
// wants a Viewer's answer passes the masked one.

export type ServiceDetailBody = components["schemas"]["ServiceDetailBody"];

export const SERVICE_ENV = "e1";
export const SERVICE_ID = "svc-api";
export const DIGEST = `sha256:${"0123456789abcdef".repeat(4)}`;

export function serviceDetail(extra: Partial<ServiceDetailBody> = {}): ServiceDetailBody {
  return {
    id: SERVICE_ID,
    name: "api",
    mode: "replicated",
    running: 2,
    desired: 3,
    stack: "",
    agent: false,
    createdAt: "2026-10-01T10:00:00Z",
    updatedAt: "2026-10-02T11:30:00Z",
    image: {
      ref: `registry.example.com/shop/api:1.4@${DIGEST}`,
      tag: "1.4",
      digest: DIGEST,
      pinned: true,
    },
    ports: [{ published: 8080, target: 80, protocol: "tcp", mode: "ingress" }],
    mounts: [
      { type: "volume", source: "api-data", target: "/data", readOnly: false },
      { type: "bind", source: "/srv/conf", target: "/etc/conf", readOnly: true },
    ],
    secrets: [{ id: "sec1", name: "db_pw", file: "db_pw.txt", uid: "0", gid: "0", mode: "0444" }],
    configs: [
      {
        id: "cfg1",
        name: "app_conf",
        file: "/etc/app.conf",
        uid: "1000",
        gid: "1000",
        mode: "0440",
      },
    ],
    networks: [{ id: "net-front", name: "shop_front", aliases: ["api", "api.internal"] }],
    env: [
      { name: "DB_PASSWORD", masked: true },
      { name: "MODE", masked: true },
    ],
    envMasked: true,
    resources: {
      cpuLimit: 0.5,
      cpuReservation: 0.25,
      memoryLimit: 268435456,
      memoryReservation: 0,
    },
    placement: {
      constraints: ["node.role==worker"],
      preferences: ["node.labels.zone"],
      maxReplicas: 2,
    },
    restartPolicy: { condition: "on-failure", delay: "5s", maxAttempts: 4, window: "2m0s" },
    updatePolicy: {
      parallelism: 2,
      delay: "10s",
      order: "start-first",
      failureAction: "pause",
      monitor: "5s",
      maxFailureRatio: 0.25,
    },
    labels: { tier: "backend" },
    updateStatus: "completed",
    updateMessage: "update completed",
    hasPreviousSpec: true,
    ...extra,
  };
}

// An answer for a service whose spec has none of the optional sections.
export function bareServiceDetail(extra: Partial<ServiceDetailBody> = {}): ServiceDetailBody {
  return serviceDetail({
    image: { ref: "nginx:1", tag: "1", digest: "", pinned: false },
    ports: [],
    mounts: [],
    secrets: [],
    configs: [],
    networks: [],
    env: [],
    envMasked: false,
    resources: { cpuLimit: 0, cpuReservation: 0, memoryLimit: 0, memoryReservation: 0 },
    placement: { constraints: [], preferences: [], maxReplicas: 0 },
    restartPolicy: undefined,
    updatePolicy: undefined,
    labels: {},
    updateStatus: "",
    updateMessage: "",
    hasPreviousSpec: false,
    ...extra,
  });
}

export type ServiceRolloutBody = components["schemas"]["RolloutBody"];

// A rollout answer: no update, unless a test says otherwise.
export function rollout(extra: Partial<ServiceRolloutBody> = {}): ServiceRolloutBody {
  return {
    state: "none",
    message: "",
    updated: 0,
    total: 0,
    latestTaskError: "",
    ...extra,
  };
}

export type ServiceSpecBody = components["schemas"]["ServiceSpecBody"];
export type ServiceReviewBody = components["schemas"]["ServiceReviewBody"];

// The version the editor tests load the service at.
export const EDIT_VERSION = 7;

// A service spec as the server states it to a caller who may not reveal secrets: the value of
// DB_PASSWORD is withheld.
export function editSpec(extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    Name: "api",
    Labels: { tier: "backend" },
    TaskTemplate: {
      ContainerSpec: {
        Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
        Env: ["MODE=prod", "DB_PASSWORD=<hidden>"],
      },
    },
    Mode: { Replicated: { Replicas: 3 } },
    ...extra,
  };
}

// What GET .../spec answers for the editor.
export function specBody(extra: Partial<ServiceSpecBody> = {}): ServiceSpecBody {
  return {
    version: EDIT_VERSION,
    name: "api",
    mode: "replicated",
    stack: "",
    agent: false,
    masked: true,
    spec: editSpec(),
    ...extra,
  };
}

// What POST .../review answers for a spec with one change in it.
export function reviewBody(extra: Partial<ServiceReviewBody> = {}): ServiceReviewBody {
  return {
    spec: editSpec(),
    version: EDIT_VERSION,
    changed: true,
    image: {
      ref: `registry.example.com/shop/api:1.4@${DIGEST}`,
      tag: "1.4",
      digest: DIGEST,
      host: "registry.example.com",
      credential: "",
      changed: false,
    },
    findings: [],
    allowed: true,
    refusal: "",
    missing: { networks: [], secrets: [], configs: [] },
    hostPolicy: { managerAllowsPrivileged: true, readOnly: false },
    warnings: [],
    ...extra,
  };
}

export interface Sent {
  method: string;
  path: string;
  body?: unknown;
}

export interface ServiceApiState {
  sent: Sent[];
  me: Me;
  detail: () => Response | Promise<Response>;
  inspect: () => Response | Promise<Response>;
  scale: () => Response | Promise<Response>;
  // The service's tasks. A test that does not read them leaves it out, and the route answers 404.
  tasks?: () => Response | Promise<Response>;
  // The service's spec and the force update and rollback writes. A test that does not use them
  // leaves them out, and the routes answer 404.
  spec?: () => Response | Promise<Response>;
  // How far the service's rolling update has got. A test that does not read it leaves it out, and
  // the route answers 404.
  rollout?: () => Response | Promise<Response>;
  forceUpdate?: () => Response | Promise<Response>;
  rollback?: () => Response | Promise<Response>;
  // The editor's review and apply. A test that does not use them leaves them out, and the
  // routes answer 404.
  review?: () => Response | Promise<Response>;
  apply?: () => Response | Promise<Response>;
}

// serviceFetch answers the page's requests: the app shell's, the service and its inspect, and
// the scale.
export function serviceFetch(state: ServiceApiState) {
  return async (req: Request): Promise<Response> => {
    const url = new URL(req.url);
    const body =
      req.method === "POST"
        ? await req
            .clone()
            .json()
            .catch(() => undefined)
        : undefined;
    state.sent.push({ method: req.method, path: url.pathname, body });
    const base = `/api/env/${SERVICE_ENV}`;
    switch (url.pathname) {
      case "/api/instance":
        return Response.json({ instanceName: "Lab", needsSetup: false, rememberDays: 30 });
      case "/api/me":
        return Response.json(state.me);
      case "/api/version":
        return Response.json({
          commit: "",
          display: "v0.1.0",
          sourceTreeUrl: "https://example.test/tree",
          sourceUrl: "https://example.test",
          version: "v0.1.0",
        });
      case "/api/environments":
        return Response.json({
          items: [
            {
              id: SERVICE_ENV,
              name: "Lab swarm",
              kind: "local",
              type: "swarm-manager",
              envType: "swarm",
              enabled: true,
              isSelfHost: true,
              createdAt: "2026-10-01T10:00:00Z",
              status: { state: "online" },
            },
          ],
          total: 1,
        });
      case `${base}/summary`:
        return Response.json({
          environment: {
            id: SERVICE_ENV,
            name: "Lab swarm",
            type: "swarm-manager",
            status: { state: "online" },
          },
          engine: { name: "Docker Engine", version: "29.1.0", apiVersion: "1.52" },
          counts: {
            containers: 1,
            containersRunning: 1,
            containersStopped: 0,
            images: 0,
            volumes: 0,
            networks: 0,
          },
          isWorker: false,
          fetchedAt: "2026-10-01T10:00:00Z",
        });
      case `${base}/services/${SERVICE_ID}`:
        return state.detail();
      case `${base}/services/${SERVICE_ID}/inspect`:
        return state.inspect();
      case `${base}/services/${SERVICE_ID}/tasks`:
        return state.tasks ? state.tasks() : new Response(null, { status: 404 });
      case `${base}/services/${SERVICE_ID}/rollout`:
        return state.rollout ? state.rollout() : new Response(null, { status: 404 });
      case `${base}/services/${SERVICE_ID}/spec`:
        return state.spec ? state.spec() : new Response(null, { status: 404 });
      case `${base}/services/${SERVICE_ID}/force-update`:
        return state.forceUpdate ? state.forceUpdate() : new Response(null, { status: 404 });
      case `${base}/services/${SERVICE_ID}/rollback`:
        return state.rollback ? state.rollback() : new Response(null, { status: 404 });
      case `${base}/services/${SERVICE_ID}/review`:
        return state.review ? state.review() : new Response(null, { status: 404 });
      case `${base}/services/${SERVICE_ID}/apply`:
        return state.apply ? state.apply() : new Response(null, { status: 404 });
      case `${base}/services/scale`:
        return state.scale();
      case `${base}/services`:
        return Response.json({
          items: [],
          total: 0,
          unfilteredTotal: 0,
          capped: false,
          statusCounts: { degraded: 0, updating: 0, problem: 0 },
          noManager: false,
          fetchedAt: "2026-10-14T09:00:00Z",
        });
    }
    return Response.json({}, { status: 404 });
  };
}
