// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useLocation } from "@tanstack/react-router";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { UpdateImagesDialog } from "@/features/stacks/update-images-dialog";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const D1 = `sha256:${"a".repeat(64)}`;
const D2 = `sha256:${"b".repeat(64)}`;
const LOCAL_ID = `sha256:${"c".repeat(64)}`;

let services: Array<Record<string, unknown>>;
let servicesStatus: number;
let reviewImages: Array<Record<string, unknown>>;
let reviewStatus: number;
let reviewType: string;
let reviews: Array<{ mode: string; services: string[] }>;
let client: QueryClient;

const row = (short: string, over: Record<string, unknown> = {}) => ({
  id: `id-${short}`,
  name: `tracer_${short}`,
  short,
  mode: "replicated",
  state: "running",
  running: 1,
  desired: 1,
  image: `nginx:1.27-${short}`,
  pinned: `nginx:1.27-${short}@${D1}`,
  pinState: "pinned",
  ...over,
});

beforeEach(() => {
  services = [row("web"), row("api")];
  servicesStatus = 200;
  reviewImages = [];
  reviewStatus = 200;
  reviewType = "";
  reviews = [];
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken("csrf");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "operator" } }));
      if (path === `/api/env/${ENV}/stacks/s1/services`) {
        if (servicesStatus !== 200) return Response.json({}, { status: servicesStatus });
        return Response.json({ items: services });
      }
      if (req.method === "POST" && path === `/api/env/${ENV}/stacks/s1/review`) {
        reviews.push((await req.json()) as { mode: string; services: string[] });
        if (reviewStatus !== 200) {
          return Response.json(
            { type: reviewType, title: "x", status: reviewStatus, detail: "" },
            { status: reviewStatus },
          );
        }
        return Response.json({
          problems: [],
          summary: {},
          images: reviewImages,
          services: [],
          objects: [],
          draftVersion: 1,
          pins: {},
          needsAdmin: [],
          yaml: { changedLines: 0, hunks: [], lines: [] },
        });
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function Where() {
  const loc = useLocation();
  return <p data-testid="where">{`${loc.pathname}${loc.searchStr}`}</p>;
}

function mount() {
  const onOpenChange = vi.fn();
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <UpdateImagesDialog
            stack={{ id: "s1", envId: ENV, name: "tracer", target: "swarm" }}
            open
            onOpenChange={onOpenChange}
          />
          <Where />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
  return onOpenChange;
}

const submit = () => screen.getByRole("button", { name: /Review image updates|Looking up…/ });
const box = (service: string) =>
  screen.getByRole("checkbox", { name: `Update the image of ${service}` });

describe("UpdateImagesDialog", () => {
  it("shows a skeleton list while the services load, then ticks every eligible one", async () => {
    mount();
    expect(screen.getByRole("heading", { name: "Update images in tracer" })).toBeTruthy();
    expect(screen.getByTestId("update-skeleton")).toBeTruthy();
    expect(await screen.findByRole("table", { name: "Services of tracer" })).toBeTruthy();
    expect(screen.queryByTestId("update-skeleton")).toBeNull();
    expect(box("web").getAttribute("data-state")).toBe("checked");
    expect(box("api").getAttribute("data-state")).toBe("checked");
    expect(screen.getByText("2 services selected")).toBeTruthy();
    // The digest the service runs now is a chip with the full digest in its name.
    expect(screen.getAllByRole("button", { name: `Pinned to ${D1}` })).toHaveLength(2);
    expect(submit().getAttribute("aria-disabled")).toBeNull();
  });

  it("lists a service pinned in the file, a local image and one that is not deployed off, with their notes", async () => {
    services = [
      row("web"),
      row("fixed", { image: `nginx@${D2}`, pinned: `nginx@${D2}` }),
      row("mine", { image: "mine:1", pinned: LOCAL_ID, pinState: "local" }),
      row("ghost", { pinned: "", pinState: "new" }),
    ];
    mount();
    await screen.findByRole("table");
    for (const [service, note] of [
      ["fixed", "Pinned in the compose file"],
      ["mine", "Not from a registry"],
      ["ghost", "Not deployed"],
    ] as const) {
      const r = box(service).closest("tr") as HTMLElement;
      expect(box(service).hasAttribute("disabled")).toBe(true);
      expect(box(service).getAttribute("data-state")).toBe("unchecked");
      expect(within(r).getByText(note)).toBeTruthy();
    }
    expect(box("web").getAttribute("data-state")).toBe("checked");
    expect(screen.getByText("1 service selected")).toBeTruthy();
  });

  it("opens the review in update mode with the chosen services in the address", async () => {
    mount();
    await screen.findByRole("table");
    fireEvent.click(box("api"));
    fireEvent.click(submit());
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(
        "/env/e1/stacks/s1/review?mode=update&services=web",
      ),
    );
    expect(reviews).toEqual([{ mode: "update", services: ["web"] }]);
  });

  it("blocks the submit with a reason when nothing is ticked, and Select all brings the ticks back", async () => {
    mount();
    await screen.findByRole("table");
    fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(screen.getByText("0 services selected")).toBeTruthy();
    expect(submit().getAttribute("aria-disabled")).toBe("true");
    // G-09-8c: aria-disabled alone does not fade the Button primitive; the call site must.
    expect(submit().className).toContain("aria-disabled:cursor-not-allowed");
    expect(submit().className).toContain("aria-disabled:opacity-50");
    expect(screen.getAllByText("Tick at least one service.").length).toBeGreaterThan(0);
    fireEvent.click(submit());
    expect(reviews).toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Select all" }));
    expect(screen.getByText("2 services selected")).toBeTruthy();
    expect(submit().getAttribute("aria-disabled")).toBeNull();
  });

  it("says no service can be updated when every one is off, and keeps the submit off", async () => {
    services = [row("fixed", { image: `nginx@${D2}`, pinned: `nginx@${D2}` })];
    mount();
    // The alert says it, and the footer gives it as the reason the submit is off.
    expect((await screen.findAllByText("No services can be updated.")).length).toBe(2);
    expect(submit().getAttribute("aria-disabled")).toBe("true");
    expect(screen.getByRole("button", { name: "Select all" }).hasAttribute("disabled")).toBe(true);
    fireEvent.click(submit());
    expect(reviews).toHaveLength(0);
  });

  it("keeps the dialog open with an alert and Retry when the services cannot be loaded", async () => {
    servicesStatus = 500;
    mount();
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't load the services of tracer.");
    servicesStatus = 200;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table")).toBeTruthy();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps the dialog open with an alert and Retry when the lookup fails, and opens the review on the retry", async () => {
    reviewStatus = 500;
    mount();
    await screen.findByRole("table");
    fireEvent.click(submit());
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't look up the images of tracer.");
    expect(alert.textContent).toContain("The server refused.");
    expect(screen.getByTestId("where").textContent).toBe("/");
    reviewStatus = 200;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByTestId("where").textContent).toContain("/review?"));
  });

  it("words a service the stack no longer has", async () => {
    reviewStatus = 422;
    reviewType = "services_invalid";
    mount();
    await screen.findByRole("table");
    fireEvent.click(submit());
    expect((await screen.findByRole("alert")).textContent).toContain(
      "A service you chose is not in the stack any more.",
    );
  });

  it("marks only the row whose registry cannot be reached, unticks it and stays open for the rest", async () => {
    reviewImages = [
      { service: "web", image: "nginx", digest: D2, current: D1, state: "new", note: "" },
      { service: "api", image: "api", digest: "", current: D1, state: "unreachable", note: "" },
    ];
    mount();
    await screen.findByRole("table");
    fireEvent.click(submit());
    const apiRow = (await waitFor(() => {
      const r = box("api").closest("tr") as HTMLElement;
      expect(within(r).getByText("Can't reach the registry")).toBeTruthy();
      return r;
    })) as HTMLElement;
    expect(box("api").getAttribute("data-state")).toBe("unchecked");
    expect(box("web").getAttribute("data-state")).toBe("checked");
    expect(
      within(box("web").closest("tr") as HTMLElement).queryByText("Can't reach the registry"),
    ).toBeNull();
    expect(apiRow).toBeTruthy();
    expect(screen.getByTestId("where").textContent).toBe("/");
    expect(screen.getByText("1 service selected")).toBeTruthy();

    // The next review is for the one that is left.
    fireEvent.click(submit());
    await waitFor(() =>
      expect(screen.getByTestId("where").textContent).toBe(
        "/env/e1/stacks/s1/review?mode=update&services=web",
      ),
    );
    expect(reviews.map((r) => r.services)).toEqual([["api", "web"], ["web"]]);
  });

  it("scrolls a long list inside its 240px box in rows that truncate", async () => {
    services = Array.from({ length: 80 }, (_, i) =>
      row(`service-${String(i).padStart(2, "0")}-with-a-long-name`),
    );
    mount();
    const table = await screen.findByRole("table");
    const list = table.closest('[data-slot="target-list"]') as HTMLElement;
    expect(list.className).toContain("max-h-60");
    expect(list.className).toContain("overflow-y-auto");
    expect(table.querySelectorAll("tbody tr")).toHaveLength(80);
    expect((table.querySelector("tbody tr") as HTMLElement).className).toContain("h-11");
    expect(screen.getByText("80 services selected")).toBeTruthy();
  });
});
