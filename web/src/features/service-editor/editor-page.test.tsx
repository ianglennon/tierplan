// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { placementRetry } from "@/api/placement";
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
  CREATED_ID,
  createdBody,
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
import { settled } from "@/test-settle";

// The edit page (plan 06-19, D-01, D-03, D-05, RES-03; UI-SPEC "Service editor", "Review and
// apply"): the Spec tab, the review, the apply with its progress, and the states around them.

const EDIT_PATH = `/env/${SERVICE_ENV}/services/${SERVICE_ID}/edit`;
// The tests of the Spec tab, the review and the apply open on the Spec tab; the Form is the
// default, and its tests ask for it.
const EDIT = `${EDIT_PATH}?tab=spec`;
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
  placementRetry.attempts = 0;
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
  placementRetry.attempts = 3;
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

  it.each([
    ["bogus", "form"],
    ["spec", "spec"],
    ["form", "form"],
  ])("opens with tab=%s in the address on the %s tab", async (tab, shown) => {
    renderAt(`${EDIT_PATH}?tab=${tab}`);
    if (shown === "spec") await ready();
    else await screen.findByLabelText("Image");
    expect(
      screen
        .getByRole("tab", { name: shown === "spec" ? "Spec" : "Form" })
        .getAttribute("aria-selected"),
    ).toBe("true");
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

  it("holds the editor inert while the review is out and frees it when the answer comes", async () => {
    let answer: (r: Response) => void = () => {};
    state.review = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    const box = await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    await waitFor(() => expect(reviewButton().textContent).toContain("Checking…"));
    const held = box.closest("[inert]");
    expect(held).not.toBeNull();
    expect(held?.getAttribute("aria-busy")).toBe("true");
    expect(screen.getByRole("tab", { name: "Form" }).closest("[inert]")).toBe(held);
    expect(screen.getByRole("tab", { name: "Spec" }).closest("[inert]")).toBe(held);
    // The footer stays out of it, so Checking… is still announced.
    expect(reviewButton().closest("[inert]")).toBeNull();
    answer(Response.json(reviewBody({ spec: withReplicas(5) })));
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    const again = await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(again.closest("[inert]")).toBeNull();
    expect(document.querySelector("[inert]")).toBeNull();
    expect(document.querySelector("[aria-busy=true]")).toBeNull();
  });

  it("frees the editor when the review is refused", async () => {
    let answer: (r: Response) => void = () => {};
    state.review = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    const box = await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    await waitFor(() => expect(box.closest("[inert]")).not.toBeNull());
    answer(problem(409, "service_changed"));
    await screen.findByRole("alert");
    expect(document.querySelector("[inert]")).toBeNull();
    expect(
      screen.getByRole("textbox", { name: "Service spec, YAML" }).closest("[inert]"),
    ).toBeNull();
  });

  it("shows in the review the spec that was judged even when the editor's text changes while the review is out (jsdom does not enforce inert, so whether edits are blocked is left to the browser check)", async () => {
    let answer: (r: Response) => void = () => {};
    state.review = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    await waitFor(() => expect(reviewButton().textContent).toContain("Checking…"));
    typeIntoEditor(yaml(withReplicas(7)));
    answer(Response.json(reviewBody({ spec: withReplicas(5) })));
    const table = await screen.findByRole("table", { name: "Changes to api" });
    const row = within(table).getByText("Replicas").closest("tr");
    expect(row?.textContent).toContain("5");
    expect(row?.textContent).not.toContain("7");
    // What was typed during the review is kept for the next one.
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    await screen.findByRole("textbox", { name: "Service spec, YAML" });
    expect(editorText()).toBe(yaml(withReplicas(7)));
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
      "<hidden> is what Coaming shows for a value it withholds, so it can't be a value. Type the real value or remove it.",
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

  it("applies the spec that was reviewed, not one edited while the review was out", async () => {
    let answer: (r: Response) => void = () => {};
    state.review = () => new Promise<Response>((r) => (answer = r));
    renderAt();
    await ready();
    typeIntoEditor(yaml(withReplicas(5)));
    fireEvent.click(reviewButton());
    await waitFor(() => expect(reviewButton().textContent).toContain("Checking…"));
    typeIntoEditor(yaml(withReplicas(7)));
    answer(Response.json(reviewBody({ spec: withReplicas(5) })));
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    await waitFor(() => expect(posts("/apply")).toHaveLength(1));
    expect(posts("/apply")[0]?.body).toEqual({ version: EDIT_VERSION, spec: withReplicas(5) });
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
    // The editor, out of sight, still holds what was typed; the review never prints it.
    const table = screen.getByRole("table", { name: "Changes to api" });
    expect(within(table).getByText("Hidden value replaced")).toBeTruthy();
    expect(table.textContent).not.toContain("brand-new-secret");
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
    await settled(queryClient);
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
    expect(router.state.location.pathname).toBe(EDIT_PATH);
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

// Objects the spec names that do not exist (plan 06-26, D-06, D-07): the review lists them, the
// values are typed there, and the apply sends them with the spec.
describe("new objects", () => {
  const withRefs = () =>
    editSpec({
      TaskTemplate: {
        ContainerSpec: {
          Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
          Env: ["MODE=prod", "DB_PASSWORD=<hidden>"],
          Secrets: [
            { SecretName: "db-pass", File: { Name: "db-pass", UID: "0", GID: "0", Mode: 292 } },
          ],
        },
        Networks: [{ Target: "front" }],
      },
    });
  const missing = { networks: ["front"], secrets: ["db-pass"], configs: [] };

  async function reviewWithRefs() {
    state.review = () => Response.json(reviewBody({ spec: withRefs(), missing }));
    renderAt();
    await ready();
    typeIntoEditor(yaml(withRefs()));
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
  }

  it("lists them on the review, blocks Apply until the secret has a value, then sends them with the spec", async () => {
    await reviewWithRefs();
    const card = within(screen.getByRole("region", { name: "New objects" }));
    expect(card.getByText("Network front")).toBeTruthy();
    const apply = () => screen.getByRole("button", { name: "Apply changes" });
    expect(apply().getAttribute("aria-disabled")).toBe("true");
    fireEvent.click(apply());
    expect(posts("/apply")).toHaveLength(0);
    fireEvent.change(card.getByLabelText("Value"), { target: { value: "hunter2" } });
    expect(apply().getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(apply());
    expect(await screen.findByRole("heading", { name: "Applying changes to api" })).toBeTruthy();
    await waitFor(() => expect(posts("/apply")).toHaveLength(1));
    expect(posts("/apply")[0]?.body).toEqual({
      version: EDIT_VERSION,
      spec: withRefs(),
      objects: [
        { kind: "network", name: "front" },
        { kind: "secret", name: "db-pass", data: btoa("hunter2") },
      ],
    });
    expect(screen.getByText("Create network front")).toBeTruthy();
    expect(screen.getByText("Create secret db-pass")).toBeTruthy();
    await screen.findByRole("heading", { name: "Update complete" });
    expect(document.body.textContent).not.toContain("hunter2");
  });

  it("says the objects made for a failed apply were removed, and keeps the edits and the typed value on the way back", async () => {
    state.apply = () =>
      Response.json(
        {
          type: "spec_rejected",
          title: "x",
          status: 422,
          detail:
            "Couldn't update api. Docker rejected these settings: bad spec The 2 objects created for this change were removed.",
          errors: [
            { location: "reason", message: "Docker rejected these settings: bad spec" },
            { location: "removed", message: "secret:db-pass" },
            { location: "removed", message: "network:front" },
          ],
        },
        { status: 422 },
      );
    await reviewWithRefs();
    const card = within(screen.getByRole("region", { name: "New objects" }));
    fireEvent.change(card.getByLabelText("Value"), { target: { value: "hunter2" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply changes" }));
    expect(
      await screen.findByText(
        "Couldn't update api. Docker rejected these settings: bad spec The 2 objects created for this change were removed.",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    // The review asks again; the typed value is still there, so the second try needs no retyping.
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    expect(
      screen.getByRole("button", { name: "Apply changes" }).getAttribute("aria-disabled"),
    ).toBeNull();
  });
});

// The Form tab (plan 06-20, D-01, D-02, D-09): the same document as the Spec tab, patched by the
// form's controls, with the caution bar for what it does not show.
describe("the Form tab", () => {
  const formImage = () => screen.findByLabelText("Image") as Promise<HTMLInputElement>;
  const tabButton = (name: "Form" | "Spec") => screen.getByRole("tab", { name });
  // Radix tabs open on the press of the mouse, not on the click.
  const openTab = (name: "Form" | "Spec") => fireEvent.mouseDown(tabButton(name), { button: 0 });

  const withCommand = () =>
    editSpec({
      TaskTemplate: {
        ContainerSpec: {
          Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
          Command: ["/bin/api"],
          Healthcheck: { Test: ["CMD", "true"], Retries: 3 },
          Env: ["MODE=prod", "DB_PASSWORD=<hidden>"],
        },
      },
    });

  it("is the tab the editor opens on, with the cards filled from the spec", async () => {
    renderAt(EDIT_PATH);
    const image = await formImage();
    expect(image.value).toBe(`registry.example.com/shop/api:1.4@${DIGEST}`);
    expect(tabButton("Form").getAttribute("aria-selected")).toBe("true");
    expect(tabButton("Spec").getAttribute("aria-selected")).toBe("false");
    expect(screen.queryByRole("textbox", { name: "Service spec, YAML" })).toBeNull();
    expect((screen.getByLabelText("Replicas") as HTMLInputElement).value).toBe("3");
    expect((screen.getByLabelText("Name") as HTMLInputElement).readOnly).toBe(true);
  });

  it("makes the address follow the tab", async () => {
    const router = renderAt(EDIT_PATH);
    await formImage();
    openTab("Spec");
    await ready();
    expect((router.state.location.search as { tab?: string }).tab).toBe("spec");
    openTab("Form");
    await formImage();
    expect((router.state.location.search as { tab?: string }).tab).toBe("form");
  });

  it("moves between the tabs with the arrow keys", async () => {
    renderAt(EDIT_PATH);
    await formImage();
    tabButton("Form").focus();
    fireEvent.keyDown(tabButton("Form"), { key: "ArrowRight" });
    await waitFor(() => expect(document.activeElement).toBe(tabButton("Spec")));
  });

  it("shows on the Spec tab what the form patched, and nothing else changed", async () => {
    state.spec = () => Response.json(specBody({ spec: withCommand() }));
    renderAt(EDIT_PATH);
    const image = await formImage();
    fireEvent.change(image, { target: { value: "registry.example.com/shop/api:1.5" } });
    openTab("Spec");
    await ready();
    const expected = withCommand();
    (
      (expected.TaskTemplate as Record<string, unknown>).ContainerSpec as Record<string, unknown>
    ).Image = "registry.example.com/shop/api:1.5";
    expect(editorText()).toBe(yaml(expected));
    expect(editorText()).toContain("DB_PASSWORD=<hidden>");
  });

  it("shows on the Form tab what was typed on the Spec tab", async () => {
    renderAt(EDIT);
    await ready();
    typeIntoEditor(yaml(withReplicas(7)));
    openTab("Form");
    expect(((await screen.findByLabelText("Replicas")) as HTMLInputElement).value).toBe("7");
  });

  it("keeps the Spec tab, says why, and moves focus to the message when the spec does not parse", async () => {
    renderAt(EDIT);
    await ready();
    typeIntoEditor("Name: [oops\n");
    openTab("Form");
    const message = await screen.findByText(
      "The spec has errors, so the form can't show it. Fix them first.",
    );
    expect(tabButton("Spec").getAttribute("aria-selected")).toBe("true");
    expect(screen.queryByLabelText("Image")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(message));
    typeIntoEditor(yaml(withReplicas(5)));
    await waitFor(() =>
      expect(
        screen.queryByText("The spec has errors, so the form can't show it. Fix them first."),
      ).toBeNull(),
    );
    openTab("Form");
    await formImage();
  });

  it("raises the caution bar for settings the form does not show, and opens the Spec tab from it", async () => {
    state.spec = () => Response.json(specBody({ spec: withCommand() }));
    renderAt(EDIT_PATH);
    await formImage();
    expect(screen.getByText(/This service has additional settings not shown here/)).toBeTruthy();
    expect(
      screen.getByText(
        "Not shown: TaskTemplate.ContainerSpec.Command, TaskTemplate.ContainerSpec.Healthcheck",
      ),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Open the Spec tab" }));
    await ready();
    expect(tabButton("Spec").getAttribute("aria-selected")).toBe("true");
  });

  it("has no caution bar when the form shows everything", async () => {
    renderAt(EDIT_PATH);
    await formImage();
    expect(screen.queryByText(/This service has additional settings/)).toBeNull();
  });

  it("reviews only the image when only the image was changed, with the hidden value sent as hidden", async () => {
    state.spec = () => Response.json(specBody({ spec: withCommand() }));
    state.review = () => Response.json(reviewBody({ spec: withCommand() }));
    renderAt(EDIT_PATH);
    const image = await formImage();
    fireEvent.change(image, { target: { value: "registry.example.com/shop/api:1.5" } });
    openTab("Spec");
    await ready();
    openTab("Form");
    await formImage();
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    const body = posts("/review")[0]?.body as { version: number; spec: Record<string, unknown> };
    const expected = withCommand();
    (
      (expected.TaskTemplate as Record<string, unknown>).ContainerSpec as Record<string, unknown>
    ).Image = "registry.example.com/shop/api:1.5";
    expect(body).toEqual({ version: EDIT_VERSION, spec: expected });
  });

  it("does not let a form with a field error be reviewed", async () => {
    renderAt(EDIT_PATH);
    await formImage();
    fireEvent.change(screen.getByLabelText("Replicas"), { target: { value: "many" } });
    expect(screen.getByText("Enter a whole number of 0 or more.")).toBeTruthy();
    await waitFor(() =>
      expect(
        (reviewButton() as HTMLButtonElement).disabled ||
          reviewButton().getAttribute("aria-disabled") === "true",
      ).toBe(true),
    );
    fireEvent.click(reviewButton());
    expect(posts("/review")).toHaveLength(0);
  });

  it("goes back to the Form from the review with the edits kept", async () => {
    state.review = () => Response.json(reviewBody({ spec: withReplicas(5) }));
    renderAt(EDIT_PATH);
    await formImage();
    fireEvent.change(screen.getByLabelText("Replicas"), { target: { value: "5" } });
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    expect(((await screen.findByLabelText("Replicas")) as HTMLInputElement).value).toBe("5");
  });

  it("asks before leaving with form changes, but not when only the tab changes", async () => {
    const router = renderAt(EDIT_PATH);
    await formImage();
    fireEvent.change(screen.getByLabelText("Replicas"), { target: { value: "5" } });
    openTab("Spec");
    await ready();
    expect(screen.queryByText("Discard changes?")).toBeNull();
    void router.history.push(PAGE);
    expect(await screen.findByText("Discard changes?")).toBeTruthy();
  });

  it("sends a masked value typed over, once, and never a real one it was not given", async () => {
    state.review = () => Response.json(reviewBody({ spec: withReplicas(3) }));
    renderAt(EDIT_PATH);
    await formImage();
    const values = screen.getAllByLabelText("Variable value") as HTMLInputElement[];
    expect(values.map((v) => v.value)).toEqual(["prod", ""]);
    fireEvent.change(values[1] as HTMLInputElement, { target: { value: "n3w" } });
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    const body = posts("/review")[0]?.body as { spec: Record<string, unknown> };
    const env = (
      (body.spec.TaskTemplate as Record<string, unknown>).ContainerSpec as Record<string, unknown>
    ).Env;
    expect(env).toEqual(["MODE=prod", "DB_PASSWORD=n3w"]);
  });
  it("shows the auto-rollback box on Review as the Update policy card's own field, and a change there asks for a fresh review", async () => {
    const rolled = editSpec({
      Mode: { Replicated: { Replicas: 5 } },
      UpdateConfig: {
        Parallelism: 1,
        Delay: 0,
        FailureAction: "rollback",
        Monitor: 5_000_000_000,
        MaxFailureRatio: 0,
        Order: "stop-first",
      },
    });
    let asked = 0;
    state.review = () =>
      Response.json(reviewBody({ spec: asked++ === 0 ? withReplicas(5) : rolled }));
    renderAt(EDIT_PATH);
    await formImage();
    fireEvent.change(screen.getByLabelText("Replicas"), { target: { value: "5" } });
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review changes", level: 2 });
    const label = "Roll back automatically if an update fails";
    const reviewBox = screen.getByRole("checkbox", { name: label });
    expect(reviewBox.getAttribute("aria-checked")).toBe("false");
    fireEvent.click(reviewBox);
    await waitFor(() => expect(posts("/review")).toHaveLength(2));
    const sent = posts("/review")[1]?.body as { spec: Record<string, unknown> };
    expect(sent.spec.UpdateConfig).toEqual(rolled.UpdateConfig);
    expect(sent.spec.Mode).toEqual({ Replicated: { Replicas: 5 } });
    expect(
      (await screen.findByRole("checkbox", { name: label })).getAttribute("aria-checked"),
    ).toBe("true");
    const table = await screen.findByRole("table", { name: "Changes to api" });
    await waitFor(() => expect(within(table).getByText("On failure")).toBeTruthy());
    expect(within(table).getByText("On failure").closest("tr")?.textContent).toContain("rollback");
    // The Form has the same value when the person goes back to it.
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    const formBox = await screen.findByRole("checkbox", { name: label });
    expect(formBox.getAttribute("aria-checked")).toBe("true");
    expect((screen.getByLabelText("Tasks at a time") as HTMLInputElement).value).toBe("1");
    expect((screen.getByLabelText("Monitor") as HTMLInputElement).value).toBe("5s");
  });

  it("offers an Operator without the reveal permission only the secrets the service holds on the server, whatever has been edited", async () => {
    state.spec = () =>
      Response.json(
        specBody({
          spec: editSpec({
            TaskTemplate: {
              ContainerSpec: {
                Image: `registry.example.com/shop/api:1.4@${DIGEST}`,
                Secrets: [
                  {
                    SecretID: "sec1",
                    SecretName: "db_pw",
                    File: { Name: "db_pw", UID: "0", GID: "0", Mode: 292 },
                  },
                ],
              },
            },
          }),
        }),
      );
    const secret = (id: string, name: string) => ({
      id,
      name,
      labels: {},
      createdAt: "2026-10-01T10:00:00Z",
      updatedAt: "2026-10-01T10:00:00Z",
      usedBy: 0,
      owner: "",
      ownerKind: "",
    });
    const served = serviceFetch(state);
    vi.stubGlobal(
      "fetch",
      vi.fn(async (req: Request) => {
        if (new URL(req.url, "http://localhost").pathname === `/api/env/${SERVICE_ENV}/secrets`) {
          const items = [secret("sec1", "db_pw"), secret("sec2", "api_key")];
          return Response.json({ items, total: 2, unfilteredTotal: 2, capped: false });
        }
        return served(req);
      }),
    );
    renderAt(EDIT_PATH);
    await formImage();
    // Take db_pw out of the edited spec and add a row: the server still holds it, so it may be
    // chosen again, and api_key may not.
    fireEvent.click(screen.getByRole("button", { name: "Remove secret 1" }));
    fireEvent.click(screen.getByRole("button", { name: "Add secret" }));
    fireEvent.click(screen.getByRole("combobox", { name: "Secret" }));
    const refused = await screen.findByRole("option", { name: /api_key/ });
    expect(refused.getAttribute("aria-disabled")).toBe("true");
    expect(refused.textContent).toContain("Needs the reveal secrets permission to add a secret");
    expect(screen.getByRole("option", { name: "db_pw" }).getAttribute("aria-disabled")).not.toBe(
      "true",
    );
  });
});

// Create a service (plan 06-27, D-04, RES-03; UI-SPEC "Service editor", "Review and apply"): the
// same page, the Form opened on the documented defaults, the review a table of what will be made,
// and the apply a create.
describe("creating a service", () => {
  const NEW = `/env/${SERVICE_ENV}/services/new`;
  const CREATED_PAGE = `/env/${SERVICE_ENV}/services/${CREATED_ID}`;

  // What the review answers for the service the tests make.
  const chatSpec = () => ({
    Name: "chat",
    TaskTemplate: {
      ContainerSpec: { Image: "nginx:1.27" },
      RestartPolicy: { Condition: "any", Delay: 5_000_000_000 },
    },
    Mode: { Replicated: { Replicas: 1 } },
    UpdateConfig: {
      Parallelism: 1,
      Delay: 0,
      FailureAction: "pause",
      Monitor: 5_000_000_000,
      MaxFailureRatio: 0,
      Order: "stop-first",
    },
  });
  const pinnedImage = {
    ref: `nginx:1.27@${DIGEST}`,
    tag: "1.27",
    digest: DIGEST,
    host: "docker.io",
    credential: "",
    changed: true,
  };

  beforeEach(() => {
    state.me = makeMe({ envs: { [SERVICE_ENV]: "admin" } });
    state.createReview = () =>
      Response.json(reviewBody({ spec: chatSpec(), version: 0, image: pinnedImage }));
    state.create = () => Response.json(createdBody(), { status: 201 });
  });

  const field = (name: string) => screen.findByLabelText(name) as Promise<HTMLInputElement>;

  async function fillIn(name = "chat", image = "nginx:1.27") {
    fireEvent.change(await field("Name"), { target: { value: name } });
    fireEvent.change(await field("Image"), { target: { value: image } });
  }

  async function reviewIt() {
    await fillIn();
    fireEvent.click(reviewButton());
    await screen.findByRole("heading", { name: "Review chat", level: 2 });
  }

  it("opens with the create title, the breadcrumb and an empty name and image, and asks nothing of the service", async () => {
    renderAt(NEW);
    expect(await screen.findByRole("heading", { name: "Create service", level: 1 })).toBeTruthy();
    expect(
      screen.getByText(
        "Set up a service Swarm runs for you. Review it before anything is created.",
      ),
    ).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Services" }).getAttribute("href")).toBe(
      `/env/${SERVICE_ENV}/services`,
    );
    expect(within(crumb).getByText("Create").getAttribute("aria-current")).toBe("page");
    expect((await field("Name")).value).toBe("");
    expect((await field("Name")).readOnly).toBe(false);
    expect((await field("Image")).value).toBe("");
    expect((await field("Replicas")).value).toBe("1");
    // A new service has no spec to read.
    expect(state.sent.filter((s) => s.path.endsWith("/spec"))).toHaveLength(0);
  });

  it("does not let a service without a name and an image be reviewed", async () => {
    renderAt(NEW);
    await field("Name");
    const blocked = () =>
      (reviewButton() as HTMLButtonElement).disabled ||
      reviewButton().getAttribute("aria-disabled") === "true";
    expect(blocked()).toBe(true);
    fireEvent.change(await field("Name"), { target: { value: "chat" } });
    expect(blocked()).toBe(true);
    fireEvent.change(await field("Image"), { target: { value: "nginx:1.27" } });
    await waitFor(() => expect(blocked()).toBe(false));
    fireEvent.change(await field("Name"), { target: { value: "has space" } });
    expect(await screen.findByText("Use letters, numbers, dot, dash or underscore.")).toBeTruthy();
    expect(blocked()).toBe(true);
    expect(posts("/review")).toHaveLength(0);
  });

  it("sends the defaults and what was typed to be reviewed, with no version", async () => {
    renderAt(NEW);
    await reviewIt();
    expect(posts("/services/review")).toHaveLength(1);
    expect(posts("/services/review")[0]?.body).toEqual({ spec: chatSpec() });
  });

  it("reviews as Review {name} with the table of what will be created and Create service to confirm", async () => {
    renderAt(NEW);
    await reviewIt();
    expect(screen.getByText("Check the service Coaming will create, then create it.")).toBeTruthy();
    const table = await screen.findByRole("table", { name: "Changes to chat" });
    const heads = within(table).getAllByRole("columnheader");
    expect(heads.map((h) => h.textContent)).toEqual(["Setting", "Value"]);
    expect(within(table).getByText("Replicas").closest("tr")?.textContent).toContain("1");
    expect(screen.getByText(`nginx:1.27@${DIGEST}`)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Apply changes" })).toBeNull();
    expect(screen.getByRole("button", { name: "Create service" })).toBeTruthy();
  });

  it("creates once, shows Creating {name}, then Created {name} with the way to the new service and a toast", async () => {
    renderAt(NEW);
    await reviewIt();
    fireEvent.click(screen.getByRole("button", { name: "Create service" }));
    expect(await screen.findByRole("heading", { name: "Creating chat" })).toBeTruthy();
    await screen.findByRole("heading", { name: "Created chat" });
    expect(posts("/services")).toHaveLength(1);
    expect(posts("/services")[0]?.body).toEqual({ spec: chatSpec() });
    expect(screen.getByText("Create service chat")).toBeTruthy();
    expect(await screen.findByText("Created service chat.")).toBeTruthy();
    expect(screen.getByRole("link", { name: "View service" }).getAttribute("href")).toBe(
      CREATED_PAGE,
    );
    // A new service has no rolling update to follow.
    expect(state.sent.filter((s) => s.path.endsWith("/rollout"))).toHaveLength(0);
  });

  it("creates the service that was reviewed, not one edited while the review was out", async () => {
    let answer: (r: Response) => void = () => {};
    state.createReview = () => new Promise<Response>((r) => (answer = r));
    renderAt(NEW);
    await fillIn();
    fireEvent.click(reviewButton());
    // A new image is looked up, so the review is on screen while the server is still judging.
    expect(await screen.findByText("Looking up the digest…")).toBeTruthy();
    fireEvent.change(await field("Name"), { target: { value: "other" } });
    answer(Response.json(reviewBody({ spec: chatSpec(), version: 0, image: pinnedImage })));
    await screen.findByRole("heading", { name: "Review chat", level: 2 });
    fireEvent.click(screen.getByRole("button", { name: "Create service" }));
    await waitFor(() => expect(posts("/services")).toHaveLength(1));
    expect(posts("/services")[0]?.body).toEqual({ spec: chatSpec() });
  });

  it("says a name that exists in words, and offers the way back with the edits kept", async () => {
    state.create = () =>
      Response.json(
        {
          type: "service_name_taken",
          title: "x",
          status: 409,
          detail: "A service named chat already exists.",
        },
        { status: 409 },
      );
    renderAt(NEW);
    await reviewIt();
    fireEvent.click(screen.getByRole("button", { name: "Create service" }));
    expect(await screen.findByText("A service named chat already exists.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Back to editing" }));
    expect((await field("Name")).value).toBe("chat");
    expect(posts("/services")).toHaveLength(1);
  });

  it("names the service by what is typed after a review is refused", async () => {
    let calls = 0;
    state.createReview = () => {
      calls += 1;
      if (calls > 1) {
        return Response.json(
          reviewBody({ spec: { ...chatSpec(), Name: "chat2" }, version: 0, image: pinnedImage }),
        );
      }
      return Response.json(
        {
          type: "service_name_taken",
          title: "x",
          status: 409,
          detail: "A service named chat already exists.",
        },
        { status: 409 },
      );
    };
    renderAt(NEW);
    await fillIn();
    fireEvent.click(reviewButton());
    expect(await screen.findByText("A service named chat already exists.")).toBeTruthy();
    // The refused spec no longer stands in for the one being edited.
    fireEvent.change(await field("Name"), { target: { value: "chat2" } });
    fireEvent.click(reviewButton());
    expect(await screen.findByRole("heading", { name: "Review chat2", level: 2 })).toBeTruthy();
    expect(screen.queryByRole("heading", { name: "Review chat", level: 2 })).toBeNull();
    expect(posts("/services/review")).toHaveLength(2);
    expect(posts("/services/review")[1]?.body).toEqual({ spec: { ...chatSpec(), Name: "chat2" } });
  });

  it("words a missing image from the review in the editor", async () => {
    state.createReview = () =>
      Response.json(
        {
          type: "image_required",
          title: "x",
          status: 422,
          detail: "Enter an image like nginx:1.27 or ghcr.io/acme/app:latest.",
        },
        { status: 422 },
      );
    renderAt(`${NEW}?tab=spec`);
    await ready();
    fireEvent.click(reviewButton());
    expect(
      await screen.findByText("Enter an image like nginx:1.27 or ghcr.io/acme/app:latest."),
    ).toBeTruthy();
  });

  it("says the objects made for a failed create were removed, as Couldn't create {name}", async () => {
    state.create = () =>
      Response.json(
        {
          type: "spec_rejected",
          title: "x",
          status: 422,
          detail: "Couldn't create chat. Docker rejected these settings: bad spec",
          errors: [
            { location: "reason", message: "Docker rejected these settings: bad spec" },
            { location: "removed", message: "secret:db-pass" },
          ],
        },
        { status: 422 },
      );
    renderAt(NEW);
    await reviewIt();
    fireEvent.click(screen.getByRole("button", { name: "Create service" }));
    expect(
      await screen.findByText(
        "Couldn't create chat. Docker rejected these settings: bad spec The 1 objects created for this change were removed.",
      ),
    ).toBeTruthy();
  });

  it("turns away a caller below Admin", async () => {
    state.me = makeMe({ envs: { [SERVICE_ENV]: "operator" } });
    renderAt(NEW);
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(posts("/services/review")).toHaveLength(0);
  });

  it("asks before it throws a new service away, with the create words", async () => {
    renderAt(NEW);
    await fillIn();
    fireEvent.click(screen.getByRole("link", { name: "Cancel" }));
    const dialog = await screen.findByRole("alertdialog", { name: "Discard changes?" });
    expect(within(dialog).getByText("The service hasn't been created.")).toBeTruthy();
  });
});
