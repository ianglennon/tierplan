// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ImageDetail } from "@/api/images";
import { Toaster } from "@/components/ui/sonner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ImageOverview } from "@/features/images/overview";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Overview tab of an image on its own: Names with Copy, Details, Labels (plan 05-14).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;
const DIGEST = `nginx@sha256:${"c".repeat(64)}`;

function detail(extra: Partial<ImageDetail> = {}): ImageDetail {
  return {
    id: ID,
    shortId: "a".repeat(12),
    names: [
      { name: "nginx:1.27", kind: "tag" },
      { name: DIGEST, kind: "digest" },
    ],
    dangling: false,
    size: 5 * 1024 * 1024,
    created: "2026-10-01T10:00:00Z",
    os: "linux",
    architecture: "arm64",
    variant: "v8",
    labels: { zeta: "z", alpha: "a<b>" },
    nodes: [{ nodeId: "n1", name: "node-1", present: true, names: ["nginx:1.27"] }],
    notReporting: [],
    ...extra,
  };
}

let client: QueryClient;
let answer: () => Response;
let me: ReturnType<typeof makeMe>;

beforeEach(() => {
  answer = () => Response.json(detail());
  me = makeMe({ envs: { [ENV]: "admin" } });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = decodeURIComponent(new URL(req.url).pathname);
      if (path === "/api/me") return Response.json(me);
      if (path === `/api/env/${ENV}/images/${ID}/used-by`) {
        return Response.json({
          items: [],
          usedByContainers: 0,
          usedByServices: 0,
          notReporting: [],
        });
      }
      if (path === "/api/environments") return Response.json({ items: [] });
      if (path === `/api/env/${ENV}/images/${ID}`) return answer();
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  toast.dismiss();
  cleanup();
  vi.unstubAllGlobals();
  client.clear();
});

function mount() {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ImageOverview envId={ENV} imageId={ID} />
          <Toaster />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("image Overview", () => {
  it("lists the tags, then the digest, with their types", async () => {
    mount();
    const names = await screen.findByRole("region", { name: "Names" });
    const rows = within(names).getAllByRole("row").slice(1);
    expect(rows.map((r) => r.textContent)).toEqual([
      expect.stringContaining("nginx:1.27"),
      expect.stringContaining(DIGEST),
    ]);
    expect(within(rows[0] as HTMLElement).getByText("Tag")).toBeTruthy();
    expect(within(rows[1] as HTMLElement).getByText("Digest")).toBeTruthy();
  });

  it("copies a name with its Copy button", async () => {
    const write = vi.fn(async () => undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: write } });
    mount();
    fireEvent.click(await screen.findByRole("button", { name: `Copy ${DIGEST}` }));
    await waitFor(() => expect(write).toHaveBeenCalledWith(DIGEST));
    expect(await screen.findByText(`Copied ${DIGEST}`)).toBeTruthy();
  });

  it("shows the details: ID, size, created, operating system and architecture with its variant", async () => {
    mount();
    const card = await screen.findByRole("region", { name: "Details" });
    expect(within(card).getByText(ID)).toBeTruthy();
    expect(within(card).getByText("5 MiB")).toBeTruthy();
    expect(within(card).getByText("linux")).toBeTruthy();
    expect(within(card).getByText("arm64/v8")).toBeTruthy();
  });

  it("says some label values are hidden when the server masked them", async () => {
    answer = () => Response.json(detail({ masked: true, labels: { token: "<hidden>" } }));
    mount();
    const labels = await screen.findByRole("region", { name: "Labels" });
    expect(within(labels).getByText(/Some label values are hidden/)).toBeTruthy();
  });

  it("says nothing about hidden labels when none were", async () => {
    mount();
    const labels = await screen.findByRole("region", { name: "Labels" });
    expect(within(labels).queryByText(/Some label values are hidden/)).toBeNull();
  });

  it("shows the labels by name as text, and No labels. when there are none", async () => {
    mount();
    const labels = await screen.findByRole("region", { name: "Labels" });
    const rows = within(labels).getAllByRole("row");
    expect(rows[0]?.textContent).toContain("alpha");
    expect(within(labels).getByText("a<b>")).toBeTruthy();
    cleanup();
    client.clear();
    answer = () => Response.json(detail({ labels: {} }));
    mount();
    expect(await screen.findByText("No labels.")).toBeTruthy();
  });

  it("shows skeleton cards while the image is read", () => {
    answer = () => new Response(new ReadableStream());
    mount();
    expect(document.querySelector('[aria-busy="true"]')).toBeTruthy();
  });
});

describe("image Overview Untag…", () => {
  const several = () =>
    detail({
      names: [
        { name: "nginx:1.27", kind: "tag" },
        { name: "nginx:latest", kind: "tag" },
        { name: DIGEST, kind: "digest" },
      ],
      nodes: [
        {
          nodeId: "n1",
          name: "node-1",
          present: true,
          names: ["nginx:1.27", "nginx:latest", DIGEST],
        },
      ],
    });

  it("offers Untag… on each tag of an image with several names, and not on a digest", async () => {
    answer = () => Response.json(several());
    mount();
    expect(await screen.findByRole("button", { name: "Untag nginx:1.27" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Untag nginx:latest" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: `Untag ${DIGEST}` })).toBeNull();
  });

  it("offers no Untag… when the image has only one name", async () => {
    mount();
    await screen.findByRole("region", { name: "Names" });
    expect(screen.queryByRole("button", { name: /^Untag / })).toBeNull();
  });

  it("opens the untag dialog for that name", async () => {
    answer = () => Response.json(several());
    mount();
    const button = await screen.findByRole("button", { name: "Untag nginx:latest" });
    // Who the caller is arrives with the page; a press before that does nothing.
    await waitFor(() => {
      fireEvent.click(button);
      expect(screen.queryByRole("alertdialog")).not.toBeNull();
    });
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText("Untag nginx:latest?")).toBeTruthy();
  });

  it("makes Untag… unavailable, with the reason, for a viewer", async () => {
    me = makeMe({ envs: { [ENV]: "viewer" } });
    answer = () => Response.json(several());
    mount();
    const find = () => screen.getByRole("button", { name: "Untag nginx:1.27" });
    await screen.findByRole("button", { name: "Untag nginx:1.27" });
    // Who the caller is arrives with the page, and the button is remade when it does.
    await waitFor(() => expect(find().getAttribute("aria-disabled")).toBe("true"));
    fireEvent.click(find());
    expect((await screen.findAllByText("Needs Operator")).length).toBeGreaterThan(0);
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });
});
