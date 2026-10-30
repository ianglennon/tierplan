// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { isLostAction, ServiceActionError } from "@/api/services";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What a refusal comes to: the sentence for the alert, and whether it is the agent's policy,
// which has a notice of its own.
export interface Refusal {
  message: string;
  agentPolicy: boolean;
}

interface RefusalContext {
  // The display name of the environment, for the words about a swarm with no manager.
  environment: string;
  // The image the service names now, said when the server did not say which it looked up.
  image: string;
  // What was being done, for the words about Docker rejecting it.
  action: "update" | "rollback";
}

// refusalOf words a force update or a rollback that did not happen, from the problem type the
// server named. A type is never shown; the manager's own words are, where the server passes them
// on, because they say what it refused.
export function refusalOf(t: Translate, err: unknown, ctx: RefusalContext): Refusal {
  const e = err instanceof ServiceActionError ? err : new ServiceActionError(0);
  const say = (message: string): Refusal => ({ message, agentPolicy: false });
  if (isLostAction(e)) return say(t("actions.refusal.lost"));
  switch (e.type) {
    case "agent_policy_denied":
      return { message: "", agentPolicy: true };
    case "image_unresolved":
      return say(
        t("services.refusal.image_unresolved", {
          ref: e.fields.get("image.ref") ?? ctx.image,
          host: e.fields.get("image.host") ?? "",
        }),
      );
    case "rollback_object_missing": {
      const kind = e.fields.get("object.kind");
      const name = e.fields.get("object.name");
      const object =
        kind && name ? `${kind} ${name}` : t("services.refusal.rollback_object_unnamed");
      return say(t("services.refusal.rollback_object_missing", { object }));
    }
    case "no_previous_spec":
      return say(t("services.rollback.noEarlier"));
    case "privileged_service": {
      const list = e.entries.map((f) => `${f.location}: ${f.message}`).join("; ");
      const key = ctx.action === "update" ? "privileged_update" : "privileged_rollback";
      return say(t(`services.refusal.${key}`, { list }));
    }
    case "secret_reveal_required": {
      const list = e.entries.map((f) => f.message).join(", ");
      const key = ctx.action === "rollback" ? "secretRevealRollback" : "secretReveal";
      return say(t(`services.refusal.${key}`, { list }));
    }
    case "spec_invalid": {
      // The manager's own words say which reference is wrong; the entry at spec carries them.
      const message = e.entries.find((f) => f.location === "spec")?.message ?? e.detail;
      if (!message) return say(t("services.refusal.generic"));
      const key = ctx.action === "rollback" ? "specInvalidRollback" : "specInvalid";
      return say(t(`services.refusal.${key}`, { message }));
    }
    case "service_changed":
    case "service_not_found":
    case "no_quorum":
    case "registry_unresolved":
      return say(t(`services.refusal.${e.type}`));
    case "no_manager_connected":
      return say(t("services.refusal.no_manager_connected", { environment: ctx.environment }));
    case "host_read_only":
      return say(t("actions.refusal.host_read_only"));
    case "agent_host_policy":
      return say(t("services.problems.agent_host_policy"));
    case "force_update_failed":
    case "rollback_failed":
    case "rollback_refused":
      if (e.detail) {
        const key = ctx.action === "update" ? "rejectedUpdate" : "rejectedRollback";
        return say(t(`services.refusal.${key}`, { message: e.detail }));
      }
      return say(t("services.refusal.generic"));
  }
  if (e.status === 403) return say(t("services.refusal.permission"));
  return say(t("services.refusal.generic"));
}
