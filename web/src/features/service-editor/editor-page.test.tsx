// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { serviceRetry } from "@/api/services";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { applyTiming } from "@/features/service-editor/apply-progress";
import { renderSpec, SPEC_FORMAT_KEY } from "@/features/spec-editor/spec-format";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import { editorText, typeIntoEditor } from "@/test-editor";
import { makeMe } from "@/test-me";
import {
  DIGEST,
  EDIT_VERSION,
  editSpec,
  reviewBody,
  rollout,
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  serviceDetail,
  serviceFetch,
  specBody,
} from "@/test-service";

// The edit page (plan 06-19, D-01, D-03, D-05, RES-03; UI-SPEC "Service editor", "Review and
// apply"): the Spec tab, the review, the apply with its progress, and the states around them.

const EDIT = `/env/${SERVICE_ENV}/services/${SERVICE_ID}/edit`;
const PAGE = `/env/${SERVICE_ENV}/services/${SERVICE_ID}`;

function problem(status: number, type: string, extra: Record<string, unknown> = {}) {
  return Response.json({ type, title: "x", status, ...extra }, { status });
}

let state: ServiceApiState;
const defaults = queryClient.getDefaultOptions();

const withReplicas = (n: number) => editSpec({ Mode: { Replicated: { Replicas: n } } });
const yaml = (spec: Record<string, unknown>) => renderSpec(spec, "yaml");

beforeEach(() => {
  localStorage.clear();
  state = {
    sent: [],
    me: makeMe({ envs: { [SERVICE_ENV]: "operator" } }),
    detail: () => Response.json(serviceDetail()),
    inspect: () => Response.json({ masked: true, document: {} }),
    scale: () => Response.json({ results: [] }),
    spec: () => Response.json(specBody()),
    review: () => Response.json(reviewBody({ spec: withReplicas(5) })),
    apply: () =>
      Response.json(
        {
          image: `registry.example.com/shop/api:1.4@${DIGEST}`,
          digest: DIGEST,
          pinned: false,
          warnings: [],
        },
        { status: 202 },
      ),
    rollout: () => Response.json(rollout({ state: "completed", updated: 3, total: 3 })),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
  serviceRetry.attempts = 0;
  applyTiming.settleMs = 10;
  setCsrfToken(null);
  FakeEventSource.reset();
  vi.stubGlobal("EventSource", FakeEventSource);
  vi.stubGlobal("fetch", vi.fn(serviceFetch(state)));
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  queryClient.clear();
  queryClient.setDefaultOptions(defaults);
  serviceRetry.attempts = 1;
  applyTiming.settleMs = 2000;
  localStorage.clear();
  document.title = "";
});

function renderAt(path = EDIT) {
  const router = createRouter({
    routeTree,
    history: createMemoryHistory({ initialEntries: [path] }),
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

const posts = (suffix: string) =>
  state.sent.filter((s) => s.method === "POST" && s.path.endsWith(suffix));

async function ready() {
  return screen.findByRole("textbox", { name: "Service spec, YAML" });
}

// The footer's primary button, whatever it says now.
const reviewButton = () => screen.getByRole("button", { name: /^(Review changes|Checking…)$/ });

async function reviewAChange() {
  await ready();
  typeIntoEditor(yaml(withReplicas(5)));
  fireEvent.click(reviewButton());
  await screen.findByRole("heading", { name: "Review changes", level: 2 });
}

describe("the page", () => {
  it("shows the title, the breadcrumb and the editor with the spec as Docker states it", async () => {
    renderAt();
    expect(await screen.findByRole("heading", { name: "Edit api", level: 1 })).toBeTruthy();
    expect(
      screen.getByText("Change its settings, review the difference, then apply."),
    ).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Services" }).getAttribute("href")).toBe(
      `/env/${SERVICE_ENV}/services`,
    );
    expect(within(crumb).getByRole("link", { name: "api" }).getAttribute("href")).toBe(PAGE);
    expect(within(crumb).getByText("Edit")).toBeTruthy();
    await ready();
    expect(editorText()).toBe(yaml(editSpec()));
    expect(editorText()).toContain("DB_PASSWORD=<hidden>");
    expect(document.title).toContain("api");
  });

  it("shows skeletons and a Review changes that cannot be used while the spec loads", async () => {
    state.spec = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByRole("status", { name: "Loading the service spec" })).toBeTruthy();
    const review = await screen.findByRole("button", { name: "Review changes" });
    expect(
      review.getAttribute("aria-disabled") ?? String((review as HTMLButtonElement).disabled),
    ).toBe("true");
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("opens in the format the person chose before", async () => {
    localStorage.setItem(SPEC_FORMAT_KEY, "json");
    renderAt();
    await screen.findByRole("textbox", { name: "Service spec, JSON" });
    expect(editorText()).toBe(renderSpec(editSpec(), "json"));
  });

  it("says the service is gone when the swarm no longer has it", async () => {
    state.spec = () => problem(404, "service_not_found");
    renderAt();
    expect(await screen.findByText("This service is gone")).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("offers to try loading again when the spec cannot be read", async () => {
    state.spec = () => problem(500, "internal");
    renderAt();
    const retry = await screen.findByRole("button", { name: `Retry loading ${SERVICE_ID}` });
    state.spec = () => Response.json(specBody());
    fireEvent.click(retry);
    await ready();
  });

  it("turns away a caller below Operator", async () => {
    state.me = makeMe({ envs: { [SERVICE_ENV]: "viewer" } });
    renderAt();
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
  });

  it("is read-only for a service a stack owns, with the way back to it", async () => {
    state.spec = () => Response.json(specBody({ stack: "shop" }));
    renderAt();
    expect(
      await screen.findByRole("heading", { name: "api is managed by stack shop" }),
    ).toBeTruthy();
    const box = await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(screen.getByRole("link", { name: "Back to api" }).getAttribute("href")).toBe(PAGE);
    expect(screen.queryByRole("button", { name: "Review changes" })).toBeNull();
  });

  it.each(["bogus", "spec", "form"])("opens with tab=%s in the address", async (tab) => {
    renderAt(`${EDIT}?tab=${tab}`);
    await ready();
  });
});

describe("the Spec tab", () => {
  it("does not let a spec that does not parse be reviewed, and says so once typing stops", async () => {
    renderAt();
    await ready();
    typeIntoEditor("Name: [oops\n");
    await waitFor(() =>
      expect(
        (reviewButton() as HTMLButtonElement).disabled ||
          reviewButton().getAttribute("aria-disabled") === "true",
      ).toBe(true),
    );
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(/^Line \d+: /)).toBeTruthy();
    expect(within(alert).getByText("Fix the spec before you review changes.")).toBeTruthy();
    fireEvent.click(reviewButton());
    expect(posts("/review")).toHaveLength(0);
  });

  it("sends the version it loaded and the spec as parsed, to be reviewed", async () => {
    renderAt();
    await ready();
    const edited = withReplicas(5);
    typeIntoEditor(yaml(edited));
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    expect(posts("/review")).toHaveLength(1);
    expect(posts("/review")[0]?.body).toEqual({ version: EDIT_VERSION, spec: edited });
  });

  it("reads Checking… while the review is out, and does not send it twice", async () => {
    let answer: (r: Response) => void = () => {};
    state.review = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    await waitFor(() => expect(reviewButton().textContent).toContain("Checking…"));
    expect(
      reviewButton().getAttribute("aria-disabled") ??
        String((reviewButton() as HTMLButtonElement).disabled),
    ).toBe("true");
    fireEvent.click(reviewButton());
    expect(posts("/review")).toHaveLength(1);
    answer(Response.json(reviewBody({ spec: withReplicas(5) })));
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
  });

  it("goes to the review at once, with the digest being looked up, when the image changed", async () => {
    let answer: (r: Response) => void = () => {};
    state.review = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    await ready();
    const retagged = editSpec({
      TaskTemplate: {
        ContainerSpec: {
          Image: "registry.example.com/shop/api:1.5",
          Env: ["MODE=prod", "DB_PASSWORD=<hidden>"],
        },
      },
    });
    typeIntoEditor(yaml(retagged));
    fireEvent.click(reviewButton());
    expect(await screen.findByText("Looking up the digest…")).toBeTruthy();
    answer(Response.json(reviewBody({ spec: retagged })));
    await waitFor(() => expect(screen.queryByText("Looking up the digest…")).toBeNull());
  });

  it("returns to the editor with the refusal and the edits kept when a review begun on a new image is refused", async () => {
    state.review = () => problem(409, "service_changed");
    renderAt();
    await ready();
    const retagged = editSpec({
      TaskTemplate: { ContainerSpec: { Image: "registry.example.com/shop/api:1.5" } },
    });
    typeIntoEditor(yaml(retagged));
    fireEvent.click(reviewButton());
    await screen.findByRole("alert");
    expect(screen.getByRole("textbox", { name: "Service spec, YAML" })).toBeTruthy();
    expect(editorText()).toBe(yaml(retagged));
  });

  it("shows the review's difference, groups, and the footer to go back or apply", async () => {
    renderAt();
    await reviewAChange();
    const table = screen.getByRole("table", { name: "Changes to api" });
    expect(within(table).getByText("Replicas")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Back to editing" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Apply changes" })).toBeTruthy();
    expect(screen.queryByRole("textbox", { name: "Service spec, YAML" })).toBeNull();
  });

  it("goes back to the editor with the edits kept", async () => {
    renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(editorText()).toBe(yaml(withReplicas(5)));
    expect(reviewButton().textContent).toContain("Review changes");
  });

  it("says a version conflict in the words of the editor and reloads the service on request", async () => {
    state.review = () => problem(409, "service_changed");
    renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    expect(
      await screen.findByText(
        "This service was changed by someone else while you were editing. Reload it, then make your changes again.",
      ),
    ).toBeTruthy();
    state.spec = () => Response.json(specBody({ version: 8, spec: withReplicas(4) }));
    state.review = () => Response.json(reviewBody({ spec: withReplicas(6), version: 8 }));
    fireEvent.click(screen.getByRole("button", { name: "Reload service" }));
    await waitFor(() => expect(editorText()).toBe(yaml(withReplicas(4))));
    expect(screen.queryByRole("alert")).toBeNull();
    typeIntoEditor(yaml(withReplicas(6)));
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    expect(posts("/review")[1]?.body).toEqual({ version: 8, spec: withReplicas(6) });
  });

  it.each([
    ["spec_invalid", 422, "The spec can't be read: unknown field Foo."],
    ["name_immutable", 422, "A service's name can't change. Create a new service instead."],
    ["mode_immutable", 422, "A service's mode can't change."],
    ["stack_label_reserved", 422, "Labels starting with com.docker.stack. are reserved."],
    [
      "placeholder_reserved",
      422,
      "<hidden> is what Bayplan shows for a value it withholds, so it can't be a value. Type the real value or remove it.",
    ],
    [
      "stack_owned",
      409,
      "This service is managed by a stack, so it can't be edited here. Change it through the stack.",
    ],
  ] as const)(
    "words the refusal %s of a review as an alert and stays on the editor",
    async (type, status, text) => {
      state.review = () =>
        problem(status, type, { errors: [{ location: "spec", message: "unknown field Foo" }] });
      renderAt();
      await ready();
      typeIntoEditor(yaml(withReplicas(5)));
      fireEvent.click(reviewButton());
      expect(await screen.findByText(text)).toBeTruthy();
      expect(screen.getByRole("textbox", { name: "Service spec, YAML" })).toBeTruthy();
      expect(document.body.textContent).not.toContain(type);
    },
  );

  it("names the path of a hidden value with nothing behind it", async () => {
    state.review = () =>
      problem(422, "placeholder_unmatched", {
        errors: [{ location: "TaskTemplate.ContainerSpec.Env[GONE]", message: "unmatched" }],
      });
    renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    expect(
      await screen.findByText(
        "A hidden value in the spec has nothing behind it: TaskTemplate.ContainerSpec.Env[GONE] isn't in the service. Type its value or remove it.",
      ),
    ).toBeTruthy();
  });
});

describe("apply", () => {
  it("sends the version and the edited spec once, then follows the update to its end", async () => {
    renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    expect(await screen.findByRole("heading", { name: "Applying changes to api" })).toBeTruthy();
    await screen.findByRole("heading", { name: "Update complete" });
    expect(posts("/apply")).toHaveLength(1);
    expect(posts("/apply")[0]?.body).toEqual({ version: EDIT_VERSION, spec: withReplicas(5) });
    expect(await screen.findByText("Updated api.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "View service" }).getAttribute("href")).toBe(PAGE);
  });

  it("sends what was typed and not the review's masked copy of it", async () => {
    renderAt();
    await ready();
    const typed = editSpec({
      TaskTemplate: {
        ContainerSpec: {
          Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
          Env: ["MODE=prod", "DB_PASSWORD=brand-new-secret"],
        },
      },
    });
    typeIntoEditor(yaml(typed));
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    expect(document.body.textContent).not.toContain("brand-new-secret");
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await screen.findByRole("heading", { name: "Applying changes to api" });
    await waitFor(() => expect(posts("/apply")).toHaveLength(1));
    expect(posts("/apply")[0]?.body).toEqual({ version: EDIT_VERSION, spec: typed });
  });

  it("reads Applying… and sends it once", async () => {
    let answer: (r: Response) => void = () => {};
    state.apply = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await screen.findByRole("heading", { name: "Applying changes to api" });
    expect(posts("/apply")).toHaveLength(1);
    answer(Response.json({ image: "x", digest: "", pinned: false, warnings: [] }, { status: 202 }));
    await screen.findByRole("heading", { name: "Update complete" });
  });

  it("says a version conflict at apply, offers Reload service, and tries nothing again", async () => {
    state.apply = () => problem(409, "service_changed");
    renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    expect(
      await screen.findByText(
        "This service was changed by someone else while you were editing. Reload it, then make your changes again.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reload service" })).toBeTruthy();
    await new Promise((r) => setTimeout(r, 100));
    expect(posts("/apply")).toHaveLength(1);
    state.spec = () => Response.json(specBody({ version: 8, spec: withReplicas(4) }));
    fireEvent.click(screen.getByRole("button", { name: "Reload service" }));
    await waitFor(() => expect(editorText()).toBe(yaml(withReplicas(4))));
  });

  it("goes back to the editor after a refusal with the edits kept", async () => {
    state.apply = () =>
      problem(422, "spec_rejected", { detail: "Docker rejected these settings: no such secret" });
    renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    expect(await screen.findByText("Docker rejected these settings: no such secret")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(editorText()).toBe(yaml(withReplicas(5)));
  });

  it("closes the editor to the service after a refusal", async () => {
    state.apply = () => problem(500, "update_failed");
    const router = renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await screen.findByRole("alert");
    fireEvent.click(screen.getByRole("button", { name: "Close editor" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(PAGE));
  });
});

describe("leaving the editor", () => {
  it("goes straight to the service when nothing was changed", async () => {
    const router = renderAt();
    await ready();
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(PAGE));
    expect(screen.queryByText("Discard changes?")).toBeNull();
  });

  it("asks before it throws changes away, and keeps them on Keep editing", async () => {
    const router = renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Discard changes?" });
    expect(within(dialog).getByText("The service hasn't been updated.")).toBeTruthy();
    fireEvent.click(within(dialog).getByRole("button", { name: "Keep editing" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    expect(router.state.location.pathname).toBe(EDIT);
    expect(editorText()).toBe(yaml(withReplicas(5)));
  });

  it("leaves when the changes are discarded", async () => {
    const router = renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Discard changes?" });
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard changes" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(PAGE));
  });

  it("asks too when the text no longer parses", async () => {
    renderAt();
    await ready();
    typeIntoEditor("Name: [oops\n");
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    await screen.findByRole("alertdialog", { name: "Discard changes?" });
  });

  it("does not ask once the change was applied", async () => {
    const router = renderAt();
    await reviewAChange();
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await screen.findByRole("heading", { name: "Update complete" });
    fireEvent.click(screen.getByRole("link", { name: "View service" }));
    await waitFor(() => expect(router.state.location.pathname).toBe(PAGE));
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
