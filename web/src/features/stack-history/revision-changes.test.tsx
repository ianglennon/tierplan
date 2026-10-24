// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import { RevisionChanges } from "@/features/stack-history/revision-changes";
import "@/i18n";

const ENV = "e1";

type Json = Record<string, unknown>;

let client: QueryClient;
let status: number;
let problemType: string;
let body: Json;
let calls: number;

const DIGEST_A = `sha256:${"ab12".repeat(16)}`;
const DIGEST_B = `sha256:${"cd34".repeat(16)}`;

// Rolling back to revision 1 would move web from 1.28 back to 1.27, and an environment value that the server
// withheld differs between the two.
function webChanged(): Json {
  const spec = (image: string) => ({
    TaskTemplate: { ContainerSpec: { Image: image, Env: ["API_KEY=<hidden>", "MODE=<hidden>"] } },
  });
  return {
    name: "tracer_web",
    short: "web",
    change: "changed",
    current: spec(`nginx:1.28-alpine@${DIGEST_B}`),
    next: spec(`nginx:1.27-alpine@${DIGEST_A}`),
    hiddenChanges: ["TaskTemplate.ContainerSpec.Env[API_KEY]"],
    settings: 2,
  };
}

beforeEach(() => {
  status = 200;
  problemType = "";
  calls = 0;
  body = {
    deployedSeq: 2,
    services: [webChanged()],
    summary: { added: 0, changed: 1, removed: 0 },
    identical: false,
  };
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === `/api/env/${ENV}/stacks/s1/revisions/1/changes`) {
        calls++;
        if (status !== 200) {
          return Response.json(
            { type: problemType, title: "Problem", status, detail: "x" },
            { status },
          );
        }
        return Response.json(body);
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

function mount(seq = 1, deployedSeq = 2) {
  return render(
    <QueryClientProvider client={client}>
      <RevisionChanges envId={ENV} stackId="s1" seq={seq} deployedSeq={deployedSeq} />
    </QueryClientProvider>,
  );
}

describe("the Changes tab", () => {
  it("captions the comparison and lists each service the roll back would change", async () => {
    mount();

    expect(
      await screen.findByText(
        "What rolling back to #1 would change, compared with the deployed revision #2.",
      ),
    ).toBeTruthy();
    expect(screen.getByText("web")).toBeTruthy();
    expect(screen.getAllByText("Changed").length).toBeGreaterThan(0);
    expect(screen.getByText("nginx:1.28-alpine")).toBeTruthy();
    expect(screen.getByText("nginx:1.27-alpine")).toBeTruthy();
    expect(calls).toBe(1);
  });

  it("shows a setting that differs only in a hidden value by its path, with no value", async () => {
    mount();

    const table = await screen.findByRole("table", { name: "Hidden changes to web" });
    expect(within(table).getByText("TaskTemplate.ContainerSpec.Env[API_KEY]")).toBeTruthy();
    expect(screen.queryByText("<hidden>")).toBeNull();
  });
});
