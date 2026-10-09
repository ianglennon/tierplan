// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ContainerDetail } from "@/api/container-detail";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Overview } from "@/features/container-detail/overview";
import "@/i18n";

// The Overview tab of a container (plan 04-10, RES-01, D-04, P3 D-15a).

const ENV = "e1";

function detail(extra: Partial<ContainerDetail> = {}): ContainerDetail {
  return {
    nodeId: "n1",
    node: "node-1",
    readOnly: false,
    id: "web-id",
    name: "web",
    image: "nginx:1.27",
    imageId: "sha256:abc",
    created: "2026-10-01T10:00:00Z",
    state: "running",
    health: "healthy",
    startedAt: "2026-10-01T10:00:05Z",
    exitCode: 0,
    oomKilled: false,
    restartCount: 2,
    command: ["nginx", "-g", "daemon off;"],
    entrypoint: [],
    restartPolicy: "no",
    restartMaxRetries: 0,
    ports: [],
    mounts: [],
    networks: [],
    labels: {},
    env: [],
    logConfig: { type: "json-file", options: [] },
    masked: false,
    isSelf: false,
    selfKind: "",
    project: "",
    stack: "",
    service: "",
    ...extra,
  };
}

let answer: () => Response;
let urls: string[];
let client: QueryClient;

beforeEach(() => {
  urls = [];
  answer = () => Response.json(detail());
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === `/api/env/${ENV}/containers/web-id`) {
        urls.push(`${url.pathname}${url.search}`);
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  return render(
    <QueryClientProvider client={client}>
      <TooltipProvider>
        <Overview envId={ENV} containerId="web-id" node="n1" />
      </TooltipProvider>
    </QueryClientProvider>,
  );
}

const card = (title: string) => screen.getByRole("region", { name: title });

describe("Overview: the tracer cards", () => {
  it("asks for the container on its node and shows skeletons until it answers", async () => {
    mount();
    expect(screen.getByRole("status").textContent).toBe("Loading the container");
    await screen.findByRole("region", { name: "State" });
    expect(urls).toEqual(["/api/env/e1/containers/web-id?nodeId=n1"]);
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("shows the State rows", async () => {
    answer = () =>
      Response.json(
        detail({
          state: "exited",
          health: "unhealthy",
          exitCode: 137,
          oomKilled: true,
          restartCount: 4,
        }),
      );
    mount();
    const state = await screen.findByRole("region", { name: "State" });
    const rows = within(state)
      .getAllByRole("row")
      .map((r) => Array.from(r.querySelectorAll("td")).map((c) => c.textContent));
    expect(rows.map((r) => r[0])).toEqual([
      "Status",
      "Health",
      "Started",
      "Finished",
      "Exit code",
      "Restarts",
      "Out of memory killed",
    ]);
    expect(rows[0]?.[1]).toBe("Exited");
    expect(rows[1]?.[1]).toBe("Unhealthy");
    expect(rows[3]?.[1]).toBe("—"); // never finished: no time
    expect(rows[4]?.[1]).toBe("137");
    expect(rows[5]?.[1]).toBe("4");
    expect(rows[6]?.[1]).toBe("Yes");
  });

  it("names the variables, hides every value and says why when the server withheld them", async () => {
    answer = () =>
      Response.json(
        detail({
          masked: true,
          env: [
            { name: "DB_PASSWORD", masked: true },
            { name: "MODE", masked: true },
          ],
        }),
      );
    mount();
    const env = await screen.findByRole("region", { name: "Environment variables" });
    const rows = within(env).getAllByRole("row");
    expect(rows.map((r) => r.querySelectorAll("td")[0]?.textContent)).toEqual([
      "DB_PASSWORD",
      "MODE",
    ]);
    expect(within(env).getAllByText("••••••••")).toHaveLength(2);
    expect(within(env).queryByRole("button")).toBeNull();
    expect(
      within(env).getByText("Values are hidden. Needs the reveal secrets permission to show them."),
    ).toBeTruthy();
  });

  it("offers a Show value toggle for each variable the server sent a value for, and no caption", async () => {
    answer = () =>
      Response.json(detail({ env: [{ name: "DB_PASSWORD", value: "hunter2", masked: false }] }));
    mount();
    const env = await screen.findByRole("region", { name: "Environment variables" });
    expect(within(env).queryByText("hunter2")).toBeNull();
    expect(within(env).queryByText(/Values are hidden/)).toBeNull();
    fireEvent.click(within(env).getByRole("button", { name: "Show value of DB_PASSWORD" }));
    expect(within(env).getByText("hunter2")).toBeTruthy();
  });

  it("says so when there are no variables", async () => {
    mount();
    const env = await screen.findByRole("region", { name: "Environment variables" });
    expect(within(env).getByText("No environment variables.")).toBeTruthy();
    await waitFor(() => expect(card("State")).toBeTruthy());
  });
});
