// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { ImageTags } from "@/features/images/image-tags";
import { repoOf, repositoriesOf } from "@/features/tags/repo";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The Tags tab of an image: the tag browser of its repository inside this environment, a
// Repository select for an image with several, and the empty state of a local-only image
// (plan 05-20, IMG-03, D-19).

const ENV = "e1";
const ID = `sha256:${"a".repeat(64)}`;

class FakeObserver {
  constructor(readonly cb: IntersectionObserverCallback) {}
  observe() {}
  unobserve() {}
  disconnect() {}
  takeRecords() {
    return [];
  }
}

function image(names: { name: string; kind: "tag" | "digest" }[]) {
  return {
    id: ID,
    shortId: "a".repeat(12),
    names,
    dangling: names.length === 0,
    size: 4096,
    created: "2026-10-01T10:00:00Z",
    os: "linux",
    architecture: "arm64",
    variant: "v8",
    labels: {},
    nodes: [{ nodeId: "n1", name: "node-1", present: true, names: [] }],
    notReporting: [],
  };
}

let me: ReturnType<typeof makeMe>;
let names: { name: string; kind: "tag" | "digest" }[];
let listed: string[];
let client: QueryClient;
let changed: string[];

beforeEach(() => {
  listed = [];
  changed = [];
  me = makeMe({ envs: { [ENV]: "viewer" } });
  names = [{ name: "ghcr.io/acme/app:1.0", kind: "tag" }];
  vi.stubGlobal("IntersectionObserver", FakeObserver);
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      const path = decodeURIComponent(url.pathname);
      if (path === "/api/me") return Response.json(me);
      if (path === "/api/environments")
        return Response.json({
          items: [{ id: ENV, name: "Lab host", envType: "standalone", enabled: true }],
          total: 1,
        });
      if (path === `/api/env/${ENV}/images/${ID}`) return Response.json(image(names));
      if (path === `/api/env/${ENV}/images/tags`) {
        listed.push(url.searchParams.get("ref") ?? "");
        return Response.json({
          repo: "acme/app",
          host: "ghcr.io",
          credential: null,
          tags: [{ name: "1.0" }, { name: "0.9" }],
          hidden: 0,
          total: 2,
          more: false,
          page: 0,
          capped: false,
          limit: 10000,
        });
      }
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

function mount(repo?: string) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <ImageTags
            envId={ENV}
            imageId={ID}
            repo={repo}
            onRepoChange={(next) => changed.push(next)}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

describe("repository of an image name", () => {
  it("drops the tag and the digest but not a registry port", () => {
    expect(repoOf("nginx:1.27")).toBe("nginx");
    expect(repoOf("localhost:5000/app:1.0")).toBe("localhost:5000/app");
    expect(repoOf("localhost:5000/app")).toBe("localhost:5000/app");
    expect(repoOf(`ghcr.io/acme/app@sha256:${"c".repeat(64)}`)).toBe("ghcr.io/acme/app");
    expect(repoOf(`nginx:1.27@sha256:${"c".repeat(64)}`)).toBe("nginx");
  });

  it("lists each repository once, in name order, and none for an unnamed image", () => {
    expect(
      repositoriesOf([
        { name: "nginx:1.27" },
        { name: "nginx:latest" },
        { name: "ghcr.io/acme/app:1" },
        { name: "<none>:<none>" },
      ]),
    ).toEqual(["nginx", "ghcr.io/acme/app"]);
    expect(repositoriesOf([])).toEqual([]);
  });
});

describe("ImageTags", () => {
  it("shows the tag browser of the image's repository, read through the environment", async () => {
    mount();
    await waitFor(() => expect(screen.getByText("1.0")).toBeTruthy());
    expect(listed).toEqual(["ghcr.io/acme/app"]);
    expect(screen.getByText("Anonymous")).toBeTruthy();
    expect(screen.queryByLabelText("Repository")).toBeNull();
    expect(screen.getByRole("columnheader", { name: "Running here" })).toBeTruthy();
  });

  it("says there is no repository to browse for an image with no name", async () => {
    names = [];
    mount();
    expect(await screen.findByText("No repository to browse")).toBeTruthy();
    expect(
      screen.getByText(
        "This image has no name or registry digest, so Bayplan can't tell where it came from.",
      ),
    ).toBeTruthy();
    expect(listed).toEqual([]);
  });

  it("offers a Repository select for an image with several repositories and tells the page which was chosen", async () => {
    names = [
      { name: "ghcr.io/acme/app:1.0", kind: "tag" },
      { name: "docker.io/acme/app:1.0", kind: "tag" },
    ];
    mount("docker.io/acme/app");
    const select = await screen.findByRole("combobox", { name: "Repository" });
    expect(select.textContent).toBe("docker.io/acme/app");
    await waitFor(() => expect(listed).toEqual(["docker.io/acme/app"]));
    fireEvent.click(select);
    fireEvent.click(await screen.findByRole("option", { name: "ghcr.io/acme/app" }));
    expect(changed).toEqual(["ghcr.io/acme/app"]);
    expect(within(document.body).queryByText("No repository to browse")).toBeNull();
  });
});

describe("ImageTags pull", () => {
  it("opens the pull form on this environment with the full reference", async () => {
    me = makeMe({ envs: { [ENV]: "operator" } });
    mount();
    await waitFor(() => expect(screen.getByText("1.0")).toBeTruthy());
    const trigger = screen.getByRole("button", { name: "Actions for 1.0" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Pull to this environment" }));
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    expect((within(dialog).getByLabelText("Image") as HTMLInputElement).value).toBe(
      "ghcr.io/acme/app:1.0",
    );
    expect(within(dialog).queryByRole("combobox", { name: "Environment" })).toBeNull();
    expect(within(dialog).getByText("Pulls on Lab host.")).toBeTruthy();
  });
});
