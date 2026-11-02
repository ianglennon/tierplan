// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { ServiceActionError, serviceRetry } from "@/api/services";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ApplyProgress, applyTiming } from "@/features/service-editor/apply-progress";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";
import {
  rollout,
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  serviceDetail,
  serviceFetch,
} from "@/test-service";
import { settled } from "@/test-settle";

// The apply step (plan 06-19, D-03, P2 D-42; UI-SPEC "Review and apply"): the ordered steps of an
// update with their state, then the rolling update, and the refusal when it did not happen.

let state: ServiceApiState;
let onBack: ReturnType<typeof vi.fn<() => void>>;
let onClose: ReturnType<typeof vi.fn<() => void>>;
let onReload: ReturnType<typeof vi.fn<() => void>>;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  state = {
    sent: [],
    me: makeMe({ envs: { [SERVICE_ENV]: "operator" } }),
    detail: () => Response.json(serviceDetail()),
    inspect: () => Response.json({ masked: true, document: {} }),
    scale: () => Response.json({ results: [] }),
    rollout: () => Response.json(rollout({ state: "completed", updated: 3, total: 3 })),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  serviceRetry.attempts = 0;
  applyTiming.settleMs = 10;
  setCsrfToken(null);
  vi.stubGlobal("fetch", vi.fn(serviceFetch(state)));
  onBack = vi.fn<() => void>();
  onClose = vi.fn<() => void>();
  onReload = vi.fn<() => void>();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  serviceRetry.attempts = 1;
  applyTiming.settleMs = 2000;
});

interface Options {
  status: "pending" | "success" | "error";
  error?: ServiceActionError;
  lookup?: boolean;
  objects?: { kind: "network" | "secret" | "config"; name: string }[];
}

function renderProgress({ status, error, lookup = true, objects }: Options) {
  return render(
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        <TestRouter>
          <ApplyProgress
            envId={SERVICE_ENV}
            serviceId={SERVICE_ID}
            name="api"
            status={status}
            error={error}
            lookup={lookup}
            objects={objects}
            onBack={onBack}
            onClose={onClose}
            onReload={onReload}
          />
        </TestRouter>
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

// stepOf is the list entry of a step and the word beside it.
function stepOf(name: string): { text: string; status: string } {
  const item = screen.getByText(name).closest("li");
  if (!item) throw new Error(`no step ${name}`);
  const status = ["Waiting", "Working", "Done", "Failed"].find((w) => within(item).queryByText(w));
  return { text: item.textContent ?? "", status: status ?? "" };
}

const fields = (pairs: Record<string, string>) => new Map(Object.entries(pairs));

describe("ApplyProgress", () => {
  it("lists the steps in order, the image first, while the update is sent", () => {
    renderProgress({ status: "pending" });
    expect(screen.getByRole("heading", { name: "Applying changes to api" })).toBeTruthy();
    const steps = within(screen.getByRole("list", { name: "Steps" })).getAllByRole("listitem");
    expect(steps.map((s) => s.textContent)).toEqual([
      expect.stringContaining("Look up the image"),
      expect.stringContaining("Update service api"),
    ]);
    expect(stepOf("Look up the image").status).toBe("Working");
    expect(stepOf("Update service api").status).toBe("Waiting");
  });

  it("leaves out the image step when the image did not change", () => {
    renderProgress({ status: "pending", lookup: false });
    expect(screen.queryByText("Look up the image")).toBeNull();
    expect(stepOf("Update service api").status).toBe("Working");
  });

  it("does not ask for the rollout until the update was accepted", () => {
    renderProgress({ status: "pending" });
    expect(state.sent.some((s) => s.path.endsWith("/rollout"))).toBe(false);
  });

  it("marks the steps done, follows the rolling update, and offers the service", async () => {
    renderProgress({ status: "success" });
    expect(stepOf("Look up the image").status).toBe("Done");
    expect(stepOf("Update service api").status).toBe("Done");
    expect(await screen.findByRole("heading", { name: "Update complete" })).toBeTruthy();
    expect(screen.getByText("api is running 2 tasks on the new settings.")).toBeTruthy();
    const link = screen.getByRole("link", { name: "View service" });
    expect(link.getAttribute("href")).toBe(`/env/${SERVICE_ENV}/services/${SERVICE_ID}`);
  });

  it("shows the rolling update while it runs and says it is complete only when it is", async () => {
    let reads = 0;
    state.rollout = () => {
      reads++;
      return Response.json(
        reads < 3
          ? rollout({ state: "updating", updated: 1, total: 3, startedAt: "2026-10-15T10:00:00Z" })
          : rollout({
              state: "completed",
              updated: 3,
              total: 3,
              startedAt: "2026-10-15T10:00:00Z",
            }),
      );
    };
    renderProgress({ status: "success" });
    expect(await screen.findByText("Updating api")).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Update complete" })).toBeNull();
    expect(screen.getByRole("link", { name: "View service" })).toBeTruthy();
  });

  it("keeps View service when Docker pauses the update, and does not call it complete", async () => {
    state.rollout = () =>
      Response.json(rollout({ state: "paused", message: "task failed", updated: 1, total: 3 }));
    renderProgress({ status: "success" });
    expect(await screen.findByText("Update paused")).toBeTruthy();
    // The component reads the rollout once more after applyTiming.settleMs before it would call a
    // settled update complete: wait for that second read, and for it to be taken in.
    const rolloutReads = () => state.sent.filter((s) => s.path.endsWith("/rollout")).length;
    await waitFor(() => expect(rolloutReads()).toBeGreaterThanOrEqual(2));
    await settled(queryClient);
    expect(screen.queryByRole("heading", { name: "Update complete" })).toBeNull();
    expect(screen.getByRole("link", { name: "View service" })).toBeTruthy();
  });

  it("settles on a change that starts no rolling update", async () => {
    state.rollout = () => Response.json(rollout({ state: "none" }));
    renderProgress({ status: "success", lookup: false });
    expect(await screen.findByRole("heading", { name: "Update complete" })).toBeTruthy();
  });

  it("shows a refusal inline, with the way back and the way out, and sends nothing again", () => {
    renderProgress({ status: "error", error: new ServiceActionError(500, "update_failed") });
    const alert = screen.getByRole("alert");
    expect(alert.textContent).toContain("Something went wrong on the server.");
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    expect(onBack).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Close editor" }));
    expect(onClose).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("link", { name: "View service" })).toBeNull();
    expect(state.sent.some((s) => s.path.endsWith("/rollout"))).toBe(false);
  });

  it("says the service was changed by someone else, and offers to reload it", () => {
    renderProgress({ status: "error", error: new ServiceActionError(409, "service_changed") });
    expect(
      screen.getByText(
        "This service was changed by someone else while you were editing. Reload it, then make your changes again.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Reload service" }));
    expect(onReload).toHaveBeenCalledTimes(1);
  });

  it("fails the image step when the digest could not be looked up", () => {
    renderProgress({
      status: "error",
      error: new ServiceActionError(
        422,
        "image_unresolved",
        fields({
          "image.ref": "registry.example.com/shop/api:1.5",
          "image.host": "registry.example.com",
        }),
      ),
    });
    expect(stepOf("Look up the image").status).toBe("Failed");
    expect(stepOf("Update service api").status).toBe("Waiting");
    expect(
      screen.getByText(
        "Couldn't look up registry.example.com/shop/api:1.5 on registry.example.com to pin it to a digest. Check the name, the tag and the credential, then try again.",
      ),
    ).toBeTruthy();
  });

  it("fails the update step when Docker rejects the settings, and says Docker's words", () => {
    renderProgress({
      status: "error",
      error: new ServiceActionError(
        422,
        "spec_rejected",
        new Map(),
        "Docker rejected these settings: secret not found: db_pw",
      ),
    });
    expect(stepOf("Look up the image").status).toBe("Done");
    expect(stepOf("Update service api").status).toBe("Failed");
    expect(
      screen.getByText("Docker rejected these settings: secret not found: db_pw"),
    ).toBeTruthy();
  });

  it("names the settings that need Admin when the server refuses them", () => {
    renderProgress({
      status: "error",
      error: new ServiceActionError(
        403,
        "privileged_service",
        fields({ "TaskTemplate.ContainerSpec.CapabilityAdd[0]": "SYS_ADMIN" }),
      ),
    });
    expect(
      screen.getByText(
        "This service uses settings that need Admin: TaskTemplate.ContainerSpec.CapabilityAdd[0]: SYS_ADMIN. Ask an administrator to apply it.",
      ),
    ).toBeTruthy();
  });

  it("shows the agent notice when the host's agent is too old", () => {
    renderProgress({ status: "error", error: new ServiceActionError(502, "agent_policy_denied") });
    expect(screen.getByText("This host's agent needs an update")).toBeTruthy();
  });

  it("says a lost connection is not known to have failed", () => {
    renderProgress({ status: "error", error: new ServiceActionError(0) });
    expect(screen.getByRole("alert").textContent).toContain(
      "The connection dropped before TierPlan answered. The list shows whether it happened.",
    );
  });

  it("asks nothing again after a failure", async () => {
    renderProgress({ status: "error", error: new ServiceActionError(409, "service_changed") });
    await settled(queryClient);
    expect(state.sent.filter((s) => s.method === "POST")).toHaveLength(0);
  });
});

describe("ApplyProgress: objects made for the change", () => {
  const objects = [
    { kind: "network" as const, name: "front" },
    { kind: "secret" as const, name: "db-pass" },
  ];
  const entries = (pairs: [string, string][]) =>
    pairs.map(([location, message]) => ({ location, message }));
  const failure = (type: string, status: number, detail: string, pairs: [string, string][] = []) =>
    new ServiceActionError(status, type, fields({}), detail, entries(pairs));

  it("shows a step for each object between the image and the update, the first one working", () => {
    renderProgress({ status: "pending", lookup: false, objects });
    const steps = within(screen.getByRole("list", { name: "Steps" })).getAllByRole("listitem");
    expect(steps.map((s) => s.textContent)).toEqual([
      expect.stringContaining("Create network front"),
      expect.stringContaining("Create secret db-pass"),
      expect.stringContaining("Update service api"),
    ]);
    expect(stepOf("Create network front").status).toBe("Working");
    expect(stepOf("Create secret db-pass").status).toBe("Waiting");
    expect(stepOf("Update service api").status).toBe("Waiting");
  });

  it("marks every step done when the update was accepted", async () => {
    renderProgress({ status: "success", lookup: false, objects });
    expect(stepOf("Create network front").status).toBe("Done");
    expect(stepOf("Create secret db-pass").status).toBe("Done");
    expect(stepOf("Update service api").status).toBe("Done");
    expect(await screen.findByRole("heading", { name: "Update complete" })).toBeTruthy();
  });

  it("says the objects were removed when the update failed after they were made, and keeps the way back", () => {
    renderProgress({
      status: "error",
      lookup: false,
      objects,
      error: failure(
        "spec_rejected",
        422,
        "Couldn't update api. Docker rejected these settings: bad spec The 2 objects created for this change were removed.",
        [
          ["reason", "Docker rejected these settings: bad spec"],
          ["removed", "secret:db-pass"],
          ["removed", "network:front"],
        ],
      ),
    });
    expect(stepOf("Create network front").status).toBe("Done");
    expect(stepOf("Create secret db-pass").status).toBe("Done");
    expect(stepOf("Update service api").status).toBe("Failed");
    expect(
      screen.getByText(
        "Couldn't update api. Docker rejected these settings: bad spec The 2 objects created for this change were removed.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    expect(onBack).toHaveBeenCalledTimes(1);
  });

  it("names what could not be removed and where to remove it", () => {
    renderProgress({
      status: "error",
      lookup: false,
      objects,
      error: failure("spec_rejected", 422, "ignored", [
        ["reason", "Docker rejected these settings: bad spec"],
        ["removed", "network:front"],
        ["notRemoved", "secret:db-pass"],
      ]),
    });
    expect(
      screen.getByText(
        "Couldn't update api. Docker rejected these settings: bad spec TierPlan couldn't remove secret db-pass. Remove them under Networks, Secrets or Configs.",
      ),
    ).toBeTruthy();
  });

  it("marks the object whose name was taken as failed and the steps after it as waiting", () => {
    renderProgress({
      status: "error",
      lookup: false,
      objects,
      error: failure(
        "name_taken",
        409,
        "Couldn't update api. A secret named db-pass already exists. The 1 objects created for this change were removed.",
        [
          ["object", "secret:db-pass"],
          ["reason", "A secret named db-pass already exists."],
          ["removed", "network:front"],
        ],
      ),
    });
    expect(stepOf("Create network front").status).toBe("Done");
    expect(stepOf("Create secret db-pass").status).toBe("Failed");
    expect(stepOf("Update service api").status).toBe("Waiting");
    expect(
      screen.getByText(
        "Couldn't update api. A secret named db-pass already exists. The 1 objects created for this change were removed.",
      ),
    ).toBeTruthy();
  });

  it("says a caller who may not create the objects needs Admin, with nothing created", () => {
    renderProgress({
      status: "error",
      lookup: false,
      objects,
      error: failure("object_permission", 403, "Needs Admin to create networks and secrets."),
    });
    expect(stepOf("Create network front").status).toBe("Failed");
    expect(stepOf("Update service api").status).toBe("Waiting");
    expect(screen.getByText("Needs Admin to create networks and secrets.")).toBeTruthy();
  });
});
