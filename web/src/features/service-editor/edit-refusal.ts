// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { ServiceActionError } from "@/api/services";
import { type Refusal, refusalOf } from "@/features/services/action-refusal";

type Translate = (key: string, options?: Record<string, unknown>) => string;

// What a review or an apply that did not happen comes to.
export interface EditRefusal extends Refusal {
  // True when the service moved on while it was being edited: the way on is to load it again.
  reload: boolean;
  // The step of an apply that did not happen: the image's lookup, or the update itself.
  step: "lookup" | "update";
  // How many of the objects the apply made before it failed (each removed again, or named in the
  // message if it could not be). The apply makes them in the order of its steps.
  made: number;
  // The object whose creation failed, as kind:name, when one did.
  failedObject?: string;
  // True when the caller may not create the kinds of object the change needs, so nothing was made.
  denied: boolean;
}

interface EditRefusalContext {
  // The display name of the environment, for the words about a swarm with no manager.
  environment: string;
  // The service's name, for the words about the objects a failed apply made and removed.
  name?: string;
  // The image the service names now, said when the server did not say which it looked up.
  image: string;
  // What was being done: an edit is an update, which is the default.
  action?: "create" | "update";
}

// Docker's words arrive after this lead on a rejected spec; the page puts its own lead there.
const REJECTED_LEAD = "Docker rejected these settings: ";

// The problem types the server names for an object it was asked to create that is not right, in
// the words it chose.
const OBJECT_PROBLEMS = new Set([
  "too_many_objects",
  "object_duplicate",
  "name_invalid",
  "value_required",
  "value_invalid",
  "value_too_large",
  "templating_invalid",
  "labels_invalid",
  "subnet_invalid",
]);

// editRefusalOf words a review or an apply that was refused, from the problem type the server
// named. A type is never shown. The settings and paths the server lists are, because they say
// what to change; none of them holds a value of the spec. An apply that failed after it made
// objects says so: that they were removed again, or which could not be.
export function editRefusalOf(t: Translate, err: unknown, ctx: EditRefusalContext): EditRefusal {
  const e = err instanceof ServiceActionError ? err : new ServiceActionError(0);
  const entries = (location: string) =>
    e.entries.filter((f) => f.location === location).map((f) => f.message);
  const removed = entries("removed");
  const notRemoved = entries("notRemoved");
  // The reason on its own, without the sentence the server put around it, so the page words the
  // whole of it.
  const reason = entries("reason")[0];
  const base =
    reason === undefined
      ? e
      : new ServiceActionError(e.status, e.type, e.fields, reason, e.entries);
  const refusal = refuse(t, base, ctx);
  const made = removed.length + notRemoved.length;
  if (made === 0) return refusal;
  const item = (o: string) => {
    const [kind = "", ...rest] = o.split(":");
    return t(`specEditor.apply.cleanup.item.${kind}`, { name: rest.join(":") });
  };
  const name = ctx.name ?? "";
  const creating = ctx.action === "create";
  return {
    ...refusal,
    made,
    message:
      notRemoved.length > 0
        ? t(`specEditor.apply.cleanup.${creating ? "createNotRemoved" : "notRemoved"}`, {
            name,
            reason: refusal.message,
            list: notRemoved.map(item).join(", "),
          })
        : t(`specEditor.apply.cleanup.${creating ? "createRemoved" : "removed"}`, {
            name,
            reason: refusal.message,
            count: made,
          }),
  };
}

function refuse(t: Translate, e: ServiceActionError, ctx: EditRefusalContext): EditRefusal {
  const say = (message: string, extra: Partial<EditRefusal> = {}): EditRefusal => ({
    message,
    agentPolicy: false,
    reload: false,
    step: "update",
    made: 0,
    denied: false,
    ...extra,
  });
  const listed = (location: string) =>
    e.entries.filter((f) => f.location === location).map((f) => f.message);
  if (e.type !== undefined && OBJECT_PROBLEMS.has(e.type)) {
    return say(e.detail || t("services.refusal.generic"));
  }
  switch (e.type) {
    case "service_changed":
      return say(t("specEditor.refusal.service_changed"), { reload: true });
    case "image_required":
      return say(t("specEditor.refusal.image_required"));
    case "service_name_taken":
      return say(e.detail || t("services.refusal.generic"));
    case "create_failed":
      return say(
        e.detail
          ? t("specEditor.refusal.create_failed", { message: e.detail })
          : t("services.refusal.generic"),
      );
    case "stack_owned":
    case "name_immutable":
    case "mode_immutable":
    case "stack_label_reserved":
    case "placeholder_reserved":
    case "nothing_to_apply":
      return say(t(`specEditor.refusal.${e.type}`));
    case "spec_invalid": {
      const message = listed("spec")[0];
      return say(
        message
          ? t("specEditor.refusal.spec_invalid", { message })
          : t("specEditor.refusal.spec_invalid_bare"),
      );
    }
    case "placeholder_unmatched":
      return say(
        t("specEditor.refusal.placeholder_unmatched", { path: e.entries[0]?.location ?? "" }),
      );
    case "objects_missing":
      return say(t("specEditor.refusal.objects_missing", { names: listed("networks").join(", ") }));
    case "privileged_service":
      return say(
        t("specEditor.review.refusal", {
          list: e.entries.map((f) => `${f.location}: ${f.message}`).join("; "),
        }),
      );
    case "spec_rejected": {
      const message = (e.detail ?? "").replace(REJECTED_LEAD, "").trim();
      return say(
        message
          ? t("specEditor.refusal.spec_rejected", { message })
          : t("services.refusal.generic"),
      );
    }
    case "update_failed":
      return say(
        e.detail
          ? t("services.refusal.rejectedUpdate", { message: e.detail })
          : t("services.refusal.generic"),
      );
    case "name_taken":
      return say(e.detail || t("services.refusal.generic"), { failedObject: listed("object")[0] });
    case "object_permission":
      return say(e.detail || t("services.refusal.generic"), { denied: true });
    case "image_unresolved":
      return {
        ...refusalOf(t, e, { environment: ctx.environment, image: ctx.image, action: "update" }),
        reload: false,
        step: "lookup",
        made: 0,
        denied: false,
      };
  }
  return {
    ...refusalOf(t, e, { environment: ctx.environment, image: ctx.image, action: "update" }),
    reload: false,
    step: "update",
    made: 0,
    denied: false,
  };
}
