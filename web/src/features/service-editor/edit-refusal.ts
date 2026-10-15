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
}

interface EditRefusalContext {
  // The display name of the environment, for the words about a swarm with no manager.
  environment: string;
  // The image the service names now, said when the server did not say which it looked up.
  image: string;
}

// Docker's words arrive after this lead on a rejected spec; the page puts its own lead there.
const REJECTED_LEAD = "Docker rejected these settings: ";

// editRefusalOf words a review or an apply that was refused, from the problem type the server
// named. A type is never shown. The settings and paths the server lists are, because they say
// what to change; none of them holds a value of the spec.
export function editRefusalOf(t: Translate, err: unknown, ctx: EditRefusalContext): EditRefusal {
  const e = err instanceof ServiceActionError ? err : new ServiceActionError(0);
  const say = (message: string, extra: Partial<EditRefusal> = {}): EditRefusal => ({
    message,
    agentPolicy: false,
    reload: false,
    step: "update",
    ...extra,
  });
  const listed = (location: string) =>
    e.entries.filter((f) => f.location === location).map((f) => f.message);
  switch (e.type) {
    case "service_changed":
      return say(t("specEditor.refusal.service_changed"), { reload: true });
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
    case "image_unresolved":
      return {
        ...refusalOf(t, e, { environment: ctx.environment, image: ctx.image, action: "update" }),
        reload: false,
        step: "lookup",
      };
  }
  return {
    ...refusalOf(t, e, { environment: ctx.environment, image: ctx.image, action: "update" }),
    reload: false,
    step: "update",
  };
}
