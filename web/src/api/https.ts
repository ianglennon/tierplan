// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { readProblem } from "@/api/problem";
import type { components } from "@/api/schema";

export type HTTPSStatus = components["schemas"]["HTTPSStatusBody"];
export type HTTPSCertificate = components["schemas"]["HTTPSCertificateBody"];

// The ["https", "status"] query: the certificate the listener serves now, whatever its source.
// It is read again every minute and when the window gets focus back, so a renewal shows up.
export const httpsStatusQuery = queryOptions({
  queryKey: ["https", "status"],
  queryFn: async (): Promise<HTTPSStatus> => {
    const { data } = await api.GET("/api/https/status");
    if (!data) throw new Error("GET /api/https/status failed");
    return data;
  },
  refetchInterval: 60_000,
  refetchOnWindowFocus: true,
});

export type HTTPSACME = components["schemas"]["HTTPSACMEBody"];
export type ACMEDirectory = components["schemas"]["HTTPSACMEDirectoryBody"];
export type ACMEAccount = components["schemas"]["HTTPSACMEAccountBody"];

// The ["https", "acme", "directory"] query: the CA's terms of service address and the profiles it
// offers. The server asks the CA for it, so callers enable it only when the card is neither paused
// nor in certificate-file mode, and it is kept a day.
export const acmeDirectoryQuery = queryOptions({
  queryKey: ["https", "acme", "directory"],
  queryFn: async (): Promise<ACMEDirectory> => {
    const { data } = await api.GET("/api/https/acme/directory");
    if (!data) throw new Error("GET /api/https/acme/directory failed");
    return data;
  },
  staleTime: 24 * 60 * 60_000,
});

// ACMEAccountError is a registration the server refused. type is the problem type
// ("acme_contact_invalid", "acme_tos_required", "acme_ca_error", ...), when it sent one, and caClass
// is the failure class of a CA error ("ca_unavailable", ...).
export class ACMEAccountError extends Error {
  readonly status: number;
  readonly type: string | undefined;
  readonly caClass: string | undefined;

  constructor(status: number, type?: string, caClass?: string) {
    super(`POST /api/https/acme/account failed with ${status}`);
    this.name = "ACMEAccountError";
    this.status = status;
    this.type = type;
    this.caClass = caClass;
  }
}

export interface RegisterACMEAccountValues {
  email: string;
  agreeTos: boolean;
}

// useRegisterACMEAccount registers the Let's Encrypt account and reads the status again, so the
// summary replaces the form.
export function useRegisterACMEAccount() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (values: RegisterACMEAccountValues): Promise<ACMEAccount> => {
      const { data, error, response } = await api.POST("/api/https/acme/account", {
        body: values.email === "" ? { agreeTos: values.agreeTos } : values,
      });
      if (!data) {
        const problem = readProblem(error, response);
        const caClass = problem.fields.find((f) => f.location === "class")?.message;
        throw new ACMEAccountError(response.status, problem.type, caClass);
      }
      return data;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["https", "status"] }),
  });
}

// ACMEProfileError is a profile save the server refused. type is the problem type
// ("acme_profile_unknown", "acme_run_active", ...), when it sent one.
export class ACMEProfileError extends Error {
  readonly status: number;
  readonly type: string | undefined;

  constructor(status: number, type?: string) {
    super(`PUT /api/https/acme/profile failed with ${status}`);
    this.name = "ACMEProfileError";
    this.status = status;
    this.type = type;
  }
}

// useSaveACMEProfile saves the certificate profile later orders ask for ("" is the CA's default)
// and reads the status again, so the saved value is the one the section compares with. The
// mutation stays pending until that read is done.
export function useSaveACMEProfile() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (profile: string): Promise<string> => {
      const { data, error, response } = await api.PUT("/api/https/acme/profile", {
        body: { profile },
      });
      if (!data) {
        const problem = readProblem(error, response);
        throw new ACMEProfileError(response.status, problem.type);
      }
      return data.profile;
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["https", "status"] }),
  });
}

export type ACMERefusal = components["schemas"]["HTTPSACMERefusalBody"];
export type ACMERunStart = components["schemas"]["HTTPSACMERunStartBody"];
export type ACMEBlock = components["schemas"]["HTTPSACMEBlockBody"];

// The six refusal codes the SPA has words for. A code outside this list is never invented into a
// refusal: the SPA shows only what the server gave and knows how to word.
export const ACME_REFUSAL_CODES: readonly ACMERefusal["code"][] = [
  "names_ip",
  "agent_url_ip",
  "names_wildcard",
  "names_not_public",
  "base_path",
  "agent_dial_host",
];

// httpsStatusPoll is the status query read as often as the run needs. While a run's stream is down the
// page reads the status every 5 seconds (UI-SPEC "Status refresh") so the end of the run still shows;
// otherwise the query keeps its own minute. A second observer does the polling and adds nothing while
// the stream is up: the query function cannot see the stream, which only the page knows about.
export function httpsStatusPoll(streamDown: boolean) {
  return { ...httpsStatusQuery, refetchInterval: streamDown ? 5_000 : false } as const;
}

// ACMERunError is an issue or renew the server refused. type is the problem type ("acme_refused",
// "acme_blocked", "acme_run_active", "acme_no_account", "acme_paused", "acme_cert_files",
// "acme_not_active", ...), when it sent one. A 422 carries every refusal; a 429 carries why and when
// an attempt is allowed again.
export class ACMERunError extends Error {
  readonly status: number;
  readonly type: string | undefined;
  readonly refusals: ACMERefusal[];
  readonly blocked: ACMEBlock | undefined;

  constructor(
    status: number,
    type?: string,
    refusals: ACMERefusal[] = [],
    blocked?: ACMEBlock,
    what = "issue",
  ) {
    super(`POST /api/https/acme/${what} failed with ${status}`);
    this.name = "ACMERunError";
    this.status = status;
    this.type = type;
    this.refusals = refusals;
    this.blocked = blocked;
  }
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// readRefusals reads the refusals of an acme_refused answer: errors[].message is the code and
// errors[].value its data. Only a known code is kept, and only the fields a refusal has, as the types
// they should be.
export function readRefusals(body: unknown): ACMERefusal[] {
  const errors = isRecord(body) && Array.isArray(body.errors) ? body.errors : [];
  const out: ACMERefusal[] = [];
  for (const e of errors) {
    if (!isRecord(e) || typeof e.message !== "string") continue;
    const code = ACME_REFUSAL_CODES.find((c) => c === e.message);
    if (!code) continue;
    const v = isRecord(e.value) ? e.value : {};
    const refusal: ACMERefusal = { code };
    if (typeof v.host === "string") refusal.host = v.host;
    if (typeof v.path === "string") refusal.path = v.path;
    if (typeof v.count === "number") refusal.count = v.count;
    if (Array.isArray(v.hosts)) refusal.hosts = v.hosts.filter((h) => typeof h === "string");
    out.push(refusal);
  }
  return out;
}

// readBlock reads an acme_blocked answer: errors[0].message is the reason and errors[0].value the
// instant an attempt is allowed again; Retry-After (seconds) is the fallback for the instant.
function readBlock(body: unknown, response: Response): ACMEBlock | undefined {
  const errors = isRecord(body) && Array.isArray(body.errors) ? body.errors : [];
  const first = errors[0];
  const reason =
    isRecord(first) && (first.message === "told_to_wait" || first.message === "failure_guard")
      ? first.message
      : undefined;
  if (!reason) return undefined;
  let until = isRecord(first) && typeof first.value === "string" ? first.value : "";
  if (Number.isNaN(Date.parse(until))) {
    const seconds = Number(response.headers.get("Retry-After"));
    until =
      Number.isFinite(seconds) && seconds > 0
        ? new Date(Date.now() + seconds * 1000).toISOString()
        : "";
  }
  return until === "" ? undefined : { reason, until };
}

async function startRun(what: "issue" | "renew"): Promise<ACMERunStart> {
  const path = what === "issue" ? "/api/https/acme/issue" : "/api/https/acme/renew";
  const { data, error, response } = await api.POST(path);
  if (data) return data;
  const problem = readProblem(error, response);
  throw new ACMERunError(
    response.status,
    problem.type,
    problem.type === "acme_refused" ? readRefusals(error) : [],
    problem.type === "acme_blocked" ? readBlock(error, response) : undefined,
    what,
  );
}

// useIssueCertificate orders a certificate. The answer is a job to follow on the run stream, or reused
// when the stored certificate already covers the names and nothing was ordered. The status is read
// again either way: a started run makes the card's state "running", a reused answer may have put
// Let's Encrypt back in use.
export function useIssueCertificate() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => startRun("issue"),
    retry: false,
    // Not awaited: the page attaches to the run at once, and the status catches up behind it.
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["https", "status"] });
    },
  });
}

// useRenewNow renews the certificate at once, for the names it already carries.
export function useRenewNow() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => startRun("renew"),
    retry: false,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["https", "status"] });
    },
  });
}
