// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

// The registry host of a normalised reference: docker.io/library/nginx:1.27 is docker.io.
export function registryHost(ref: string): string {
  return ref.split("/")[0] ?? ref;
}

// The i18n key of the sentence for a failure code. A code the SPA does not know gets the
// generic sentence: a code is never shown as it is.
const FAILURE_KEYS: Record<string, string> = {
  image_not_found: "notFound",
  invalid_reference: "invalidReference",
  rate_limited: "rateLimited",
  registry_unreachable: "unreachable",
  registry_tls: "tls",
  registry_http: "http",
  node_unreachable: "nodeUnreachable",
  host_read_only: "hostReadOnly",
  cancelled: "cancelled",
  interrupted: "interrupted",
};

// A refusal to sign in or to pull, the two codes that name the credential used.
const REFUSALS = new Set(["pull_denied", "pull_unauthorized"]);

export type Translate = (key: string, options?: Record<string, unknown>) => string;

// The credential a pull used, as the job carries it: never a secret or a header.
export interface FailureCredential {
  id: string;
  name: string;
  layer: "instance" | "environment";
}

export interface FailureContext {
  // The credential the pull used, or null for an anonymous pull.
  credential: FailureCredential | null;
  // The name of the environment, said as the scope of an environment credential.
  envName?: string;
  // The normalised reference being pulled.
  ref: string;
  // The node the sentence is about.
  node: string;
}

// What a failure sentence offers to do next: open a credential in the Registries page.
// "test" opens the one that was used, "add" opens the form for a new one (which only an
// instance administrator has, so the row offers it to them alone).
export interface FailureAction {
  kind: "test" | "add";
  label: string;
  to: "/registries";
  search: { credential: string };
}

export interface FailureText {
  text: string;
  action?: FailureAction;
}

// scopeOf is the credential's scope in words: Instance, or the environment's name.
export function scopeOf(t: Translate, c: FailureCredential, envName?: string): string {
  return c.layer === "instance" ? t("registries.tester.scope.instance") : (envName ?? "");
}

// failureCopy is the sentence for a failed node and what to do next: the words for the code,
// with the node, the registry, the reference and the credential filled in (UI-SPEC "Shared
// refusals and failures"). It never includes anything the server or the registry said.
export function failureCopy(t: Translate, code: string, ctx: FailureContext): FailureText {
  const host = registryHost(ctx.ref);
  if (REFUSALS.has(code)) {
    if (ctx.credential) {
      return {
        text: t("pulls.failure.deniedWithCredential", {
          name: ctx.credential.name,
          scope: scopeOf(t, ctx.credential, ctx.envName),
        }),
        action: {
          kind: "test",
          label: t("pulls.failure.testCredential"),
          to: "/registries",
          search: { credential: ctx.credential.id },
        },
      };
    }
    return {
      text: t("pulls.failure.noCredential", { ref: ctx.ref }),
      action: {
        kind: "add",
        label: t("pulls.failure.addCredential"),
        to: "/registries",
        search: { credential: "new" },
      },
    };
  }
  const key = FAILURE_KEYS[code] ?? "generic";
  const hub = key === "rateLimited" && host === "docker.io" ? "rateLimitedHub" : key;
  return { text: t(`pulls.failure.${hub}`, { node: ctx.node, ref: ctx.ref, host }) };
}
