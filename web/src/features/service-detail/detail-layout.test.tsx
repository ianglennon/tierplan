// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { queryClient } from "@/api/query-client";
import { FakeEventSource } from "@/features/dashboard/fake-event-source";
import { routeTree } from "@/routeTree.gen";
import "@/i18n";
import {
  bareServiceDetail,
  DIGEST,
  SERVICE_ENV,
  SERVICE_ID,
  type ServiceApiState,
  type ServiceDetailBody,
  serviceDetail,
  serviceFetch,
} from "@/test-service";
import { makeMe } from "@/test-me";

// The service page: header, alerts, tabs, and the states of loading, failing and being gone
// (plan 06-06, RES-02, D-05, UI-SPEC "Service detail").

const PAGE = `/env/${SERVICE_ENV}/services/${SERVICE_ID}`;

function problem(status: number, type: string) {
  return Response.json({ type, title: "x", status }, { status });
}

let state: ServiceApiState;
const defaults = queryClient.getDefaultOptions();

beforeEach(() => {
  state = {
    sent: [],
    me: makeMe({ envs: { [SERVICE_ENV]: "operator" } }),
    detail: () => Response.json(serviceDetail()),
    inspect: () => Response.json({ masked: true, document: { ID: SERVICE_ID } }),
    scale: () =>
      Response.json({
        results: [
          { serviceId: SERVICE_ID, name: "api", result: "done", replicas: 5, changed: true },
        ],
      }),
  };
  queryClient.setDefaultOptions({ queries: { retry: false, refetchOnWindowFocus: false } });
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
  document.title = "";
});

function renderAt(path = PAGE) {
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

function answerWith(body: ServiceDetailBody) {
  state.detail = () => Response.json(body);
}

describe("the page frame", () => {
  it("shows the breadcrumb, the name with its state and badges, and the image line", async () => {
    renderAt();
    expect(await screen.findByRole("heading", { name: "api" })).toBeTruthy();
    const crumb = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(crumb).getByRole("link", { name: "Services" }).getAttribute("href")).toBe(
      "/env/e1/services",
    );
    expect(crumb.textContent).toContain("api");
    // Two of three tasks are running.
    expect(screen.getAllByText("Degraded").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("Replicated").length).toBeGreaterThanOrEqual(1);
    // The tag in mono and the short digest as a chip; the full digest is not on the line.
    expect(screen.getByText("1.4")).toBeTruthy();
    const chip = screen.getByRole("button", { name: "Copy digest" });
    expect(chip.textContent).toBe(DIGEST.slice(0, 19));
    expect(screen.getByText(/Updated/)).toBeTruthy();
    expect(document.title).toBe("api · Lab");
  });

  it("asks for the service by the ID in the address", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "api" });
    expect(state.sent.some((s) => s.path === `/api/env/e1/services/${SERVICE_ID}`)).toBe(true);
  });

  it("has an Overview tab and an Inspect tab, the first at the page's own address", async () => {
    renderAt();
    const overview = await screen.findByRole("tab", { name: "Overview" });
    expect(overview.getAttribute("href")).toBe(`/env/e1/services/${SERVICE_ID}`);
    await waitFor(() => expect(overview.getAttribute("data-active")).toBeTruthy());
    expect(screen.getByRole("tab", { name: "Inspect" }).getAttribute("href")).toBe(
      `/env/e1/services/${SERVICE_ID}/inspect`,
    );
  });

  it("shows the tabs and skeletons at once, before the service has loaded", async () => {
    state.detail = () => new Promise<Response>(() => {});
    renderAt();
    expect(await screen.findByRole("tab", { name: "Overview" })).toBeTruthy();
    expect(screen.getAllByRole("status").some((s) => s.textContent === "Loading the service")).toBe(
      true,
    );
    expect(screen.queryByRole("heading", { name: "api" })).toBeNull();
  });

  it("copies the digest from the chip", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    renderAt();
    fireEvent.click(await screen.findByRole("button", { name: "Copy digest" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(DIGEST));
  });

  it("leaves the digest chip out of an image that is not pinned", async () => {
    answerWith(bareServiceDetail());
    renderAt();
    await screen.findByRole("heading", { name: "api" });
    expect(screen.queryByRole("button", { name: "Copy digest" })).toBeNull();
    expect(screen.getByText("1")).toBeTruthy();
  });
});

describe("the alerts", () => {
  it("shows the info notice for a stack-owned service, with the stack badge", async () => {
    answerWith(serviceDetail({ stack: "shop" }));
    renderAt();
    expect(await screen.findByText("Managed by stack shop")).toBeTruthy();
    expect(
      screen.getByText(
        "Change its settings through the stack so the next deploy doesn't overwrite them. You can still scale, force update and roll back here, but a stack deploy sets those again.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("Stack shop")).toBeTruthy();
  });

  it("shows no notice for a service made on its own", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "api" });
    expect(screen.queryByText(/Managed by stack/)).toBeNull();
    expect(screen.queryByText(/Stack /)).toBeNull();
  });

  it("warns on Bayplan's agent service, and badges it", async () => {
    answerWith(serviceDetail({ agent: true, name: "bayplan-agent" }));
    renderAt();
    expect(
      await screen.findByText(
        "This is Bayplan's agent service. Removing or changing it can cut Bayplan off from every node it runs on.",
      ),
    ).toBeTruthy();
    expect(screen.getAllByText("Bayplan agent").length).toBeGreaterThanOrEqual(1);
  });
});

describe("the header actions", () => {
  it("offers Scale… to an Operator, opening the scale dialog for this service", async () => {
    renderAt();
    const scale = await screen.findByRole("button", { name: "Scale…" });
    expect(scale.getAttribute("aria-disabled")).toBeNull();
    fireEvent.click(scale);
    expect(await screen.findByRole("dialog", { name: "Scale api" })).toBeTruthy();
  });

  it("shows Scale… to a Viewer unavailable, with the reason", async () => {
    state.me = makeMe({ envs: { [SERVICE_ENV]: "viewer" } });
    renderAt();
    const scale = await screen.findByRole("button", { name: "Scale…" });
    expect(scale.getAttribute("aria-disabled")).toBe("true");
    expect(screen.getAllByText("Needs Operator").length).toBeGreaterThan(0);
    fireEvent.click(scale);
    expect(screen.queryByRole("dialog", { name: "Scale api" })).toBeNull();
  });

  it("hides Scale… for a global service and a job", async () => {
    for (const mode of ["global", "replicated-job", "global-job"] as const) {
      answerWith(serviceDetail({ mode }));
      renderAt();
      await screen.findByRole("heading", { name: "api" });
      expect(screen.queryByRole("button", { name: "Scale…" })).toBeNull();
      cleanup();
      queryClient.clear();
    }
  });

  it("copies the service ID from More", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText } });
    renderAt();
    const more = await screen.findByRole("button", { name: "More actions" });
    await act(async () => {
      fireEvent.keyDown(more, { key: "Enter" });
    });
    fireEvent.click(await screen.findByRole("menuitem", { name: "Copy service ID" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(SERVICE_ID));
  });
});

describe("the error and gone states", () => {
  it("says it couldn't load the service, and tries again on Retry", async () => {
    state.detail = () => problem(500, "internal");
    renderAt();
    expect(await screen.findByText(`Couldn't load ${SERVICE_ID}.`)).toBeTruthy();
    state.detail = () => Response.json(serviceDetail());
    fireEvent.click(screen.getByRole("button", { name: `Retry loading ${SERVICE_ID}` }));
    expect(await screen.findByRole("heading", { name: "api" })).toBeTruthy();
  });

  it("turns into the gone state when the swarm has no such service", async () => {
    state.detail = () => problem(404, "service_not_found");
    renderAt();
    expect(await screen.findByRole("heading", { name: "This service is gone" })).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to services" }).getAttribute("href")).toBe(
      "/env/e1/services",
    );
    expect(screen.queryByRole("tab", { name: "Overview" })).toBeNull();
  });

  it("turns into the gone state without a reload when the service is removed while open", async () => {
    renderAt();
    await screen.findByRole("heading", { name: "api" });
    state.detail = () => problem(404, "service_not_found");
    await act(async () => {
      await queryClient.invalidateQueries({ queryKey: ["env", SERVICE_ENV, "services"] });
    });
    expect(await screen.findByRole("heading", { name: "This service is gone" })).toBeTruthy();
  });
});

describe("the Inspect tab", () => {
  it("shows the document the server sent, with the masking notice, and names the download", async () => {
    renderAt(`${PAGE}/inspect`);
    expect(
      await screen.findByText(
        "Environment variable values are hidden. Needs the reveal secrets permission to show them.",
      ),
    ).toBeTruthy();
    expect(document.body.textContent).toContain('"ID": "svc-api"');
    expect(screen.getByRole("button", { name: "Download JSON" })).toBeTruthy();
  });

  it("shows no notice when nothing was withheld", async () => {
    state.inspect = () => Response.json({ masked: false, document: { ID: SERVICE_ID } });
    renderAt(`${PAGE}/inspect`);
    await screen.findByRole("button", { name: "Download JSON" });
    expect(screen.queryByText(/Environment variable values are hidden/)).toBeNull();
  });

  it("says it couldn't load the inspect output, with Retry", async () => {
    state.inspect = () => problem(500, "internal");
    renderAt(`${PAGE}/inspect`);
    expect(await screen.findByText(/Couldn't load the inspect output of/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});
