// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TooltipProvider } from "@/components/ui/tooltip";
import { ImageLayers } from "@/features/images/layers";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Layers tab of an image: the columns, the caption, zero-size layers, long commands
// and an agent too old for the history route (plan 05-14, D-18).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;

interface Layer {
  id: string;
  createdBy: string;
  size: number;
  created?: string;
  comment: string;
}

function layer(extra: Partial<Layer> = {}): Layer {
  return {
    id: `sha256:${"b".repeat(64)}`,
    createdBy: "RUN apt-get update",
    size: 2048,
    created: "2026-10-01T10:00:00Z",
    comment: "",
    ...extra,
  };
}

let client: QueryClient;
let answer: () => Response;
let sent: string[];

beforeEach(() => {
  sent = [];
  answer = () => Response.json({ layers: [layer()] });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = decodeURIComponent(url.pathname);
      sent.push(path + url.search);
      if (path === "/api/me") return Response.json(makeMe({ envs: { [ENV]: "admin" } }));
      if (path === "/api/environments")
        return Response.json({
          items: [
            {
              id: ENV,
              name: "Lab host",
              kind: "local",
              type: "standalone",
              envType: "standalone",
              enabled: true,
              isSelfHost: true,
              createdAt: "2026-10-01T10:00:00Z",
              status: { state: "online" },
            },
          ],
          total: 1,
        });
      if (path === `/api/env/${ENV}/images/${ID}/history`) return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  client.clear();
});

function mount(node?: string) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ImageLayers envId={ENV} imageId={ID} node={node} />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("image Layers", () => {
  it("lists Layer, Created by, Size and Created, with the 12-character ID and a caption", async () => {
    answer = () =>
      Response.json({
        layers: [layer({ size: 2048 }), layer({ id: "<missing>", createdBy: "ADD rootfs", size: 1024 })],
      });
    mount();
    const table = await screen.findByRole("table", { name: "Layers" });
    const heads = within(table)
      .getAllByRole("columnheader")
      .map((h) => h.textContent);
    expect(heads).toEqual(["Layer", "Created by", "Size", "Created"]);
    const rows = within(table).getAllByRole("row").slice(1);
    expect(within(rows[0] as HTMLElement).getByText("bbbbbbbbbbbb")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("—")).toBeTruthy();
    expect(within(rows[0] as HTMLElement).getByText("2 KiB")).toBeTruthy();
    expect(await screen.findByText("2 layers, 3 KiB in total")).toBeTruthy();
  });

  it("reads one layer in the singular", async () => {
    mount();
    expect(await screen.findByText("1 layer, 2 KiB in total")).toBeTruthy();
  });

  it("shows a zero-size layer as a muted 0 B", async () => {
    answer = () => Response.json({ layers: [layer({ size: 0, createdBy: "CMD [\"sh\"]" })] });
    mount();
    const zero = await screen.findByText("0 B");
    expect(zero.className).toContain("text-muted-foreground");
  });

  it("shows the commands as text, not markup", async () => {
    answer = () => Response.json({ layers: [layer({ createdBy: "RUN echo <b>bold</b>" })] });
    mount();
    expect(await screen.findByText("RUN echo <b>bold</b>")).toBeTruthy();
    expect(document.querySelector("b")).toBeNull();
  });

  it("clamps a long command to 3 lines and shows it whole with Show more", async () => {
    vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.getAttribute("data-slot") === "layer-command" ? 200 : 0;
    });
    vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockImplementation(function (
      this: HTMLElement,
    ) {
      return this.getAttribute("data-slot") === "layer-command" ? 60 : 0;
    });
    answer = () => Response.json({ layers: [layer({ createdBy: "RUN ".concat("x".repeat(600)) })] });
    mount();
    const command = await screen.findByText(/^RUN x+$/);
    expect(command.className).toContain("line-clamp-3");
    const more = await screen.findByRole("button", { name: "Show more" });
    expect(more.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(more);
    await waitFor(() => expect(command.className).not.toContain("line-clamp-3"));
    const less = screen.getByRole("button", { name: "Show less" });
    expect(less.getAttribute("aria-expanded")).toBe("true");
  });

  it("offers no Show more for a short command", async () => {
    mount();
    await screen.findByText("RUN apt-get update");
    expect(screen.queryByRole("button", { name: "Show more" })).toBeNull();
  });

  it("scrolls inside its own region", async () => {
    mount();
    const region = await screen.findByRole("region", { name: "Layers of the image" });
    expect(region.className).toContain("overflow-auto");
  });

  it("shows the agent-update notice in place of the table when the agent refuses the route", async () => {
    answer = () =>
      Response.json({ type: "agent_policy_denied", title: "x", status: 502 }, { status: 502 });
    mount();
    expect(await screen.findByText(/agent/i)).toBeTruthy();
    expect(screen.getByRole("alert")).toBeTruthy();
    expect(screen.queryByRole("table", { name: "Layers" })).toBeNull();
  });

  it("says it could not load the layers, with Retry, for any other failure", async () => {
    let fail = true;
    answer = () =>
      fail
        ? Response.json({ title: "x", status: 503 }, { status: 503 })
        : Response.json({ layers: [layer()] });
    mount();
    expect(await screen.findByText(/Couldn't load the layers/)).toBeTruthy();
    fail = false;
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("table", { name: "Layers" })).toBeTruthy();
  });

  it("passes the node on to the read", async () => {
    mount("n1");
    await screen.findByRole("table", { name: "Layers" });
    expect(sent.some((s) => s.endsWith("/history?node=n1"))).toBe(true);
  });

  it("has a skeleton while the layers are read", () => {
    answer = () => new Response(new ReadableStream());
    mount();
    expect(document.querySelector('[aria-busy="true"]')).toBeTruthy();
  });

  it("says there are no layers for an image the engine gives no history", async () => {
    answer = () => Response.json({ layers: [] });
    mount();
    expect(await screen.findByText("No layers.")).toBeTruthy();
  });
});
