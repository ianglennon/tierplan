// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { TooltipProvider } from "@/components/ui/tooltip";
import { BrowsePage } from "@/features/tags/browse-page";
import { TagBrowser } from "@/features/tags/tag-browser";
import "@/i18n";
import { makeMe } from "@/test-me";
import { TestRouter } from "@/test-router";

// The tag browser (plan 05-18, IMG-03, D-19, D-24a): a repository's tag names, the credential
// they were read with, and a mapped message in place of the table when the registry says no.

interface Sent {
  path: string;
  query: Record<string, string>;
}

interface DetailsBody {
  ref: string;
  envId?: string;
  tags: string[];
}

// jsdom has no IntersectionObserver: this one reports a row only when a test says it has
// come into view.
class FakeObserver {
  static all: FakeObserver[] = [];
  readonly els = new Set<Element>();
  constructor(readonly cb: IntersectionObserverCallback) {
    FakeObserver.all.push(this);
  }
  observe(el: Element) {
    this.els.add(el);
  }
  unobserve(el: Element) {
    this.els.delete(el);
  }
  disconnect() {
    this.els.clear();
  }
  takeRecords() {
    return [];
  }
}

// scrollTo says that the rows with these tag names are (or are no longer) on screen.
function scrollTo(names: string[], isIntersecting = true) {
  act(() => {
    for (const io of FakeObserver.all) {
      const entries = [...io.els]
        .filter((el) => names.includes(el.getAttribute("data-tag") ?? ""))
        .map((target) => ({ target, isIntersecting }) as IntersectionObserverEntry);
      if (entries.length > 0) io.cb(entries, io as unknown as IntersectionObserver);
    }
  });
}

const D1 = `sha256:${"a1".repeat(32)}`;
const D2 = `sha256:${"b2".repeat(32)}`;
const AMD = `sha256:${"c3".repeat(32)}`;
const ARM = `sha256:${"d4".repeat(32)}`;

function platform(arch: string, digest: string, over: Record<string, unknown> = {}) {
  return { os: "linux", arch, variant: "", digest, size: 5_242_880, ...over };
}

function detail(tag: string, over: Record<string, unknown> = {}) {
  return {
    tag,
    digest: D1,
    created: new Date(Date.now() - 3_600_000).toISOString(),
    platforms: [platform("amd64", AMD)],
    code: "",
    runningIn: [],
    ...over,
  };
}

let sent: Sent[];
let answer: (req: Sent) => Response | Promise<Response>;
// The details requests the browser sent, and what answers them.
let detailsSent: DetailsBody[];
let detailsAnswer: (body: DetailsBody) => Response | Promise<Response>;
let me: ReturnType<typeof makeMe>;
let client: QueryClient;

const instanceCred = {
  id: "c1",
  name: "ghcr-acme",
  scope: { kind: "instance", envId: null, envName: null },
  host: "ghcr.io",
  prefix: "acme",
};

function list(over: Record<string, unknown> = {}) {
  return {
    repo: "acme/app",
    host: "ghcr.io",
    credential: instanceCred,
    tags: [{ name: "v1.10.0" }, { name: "v1.9.2" }, { name: "latest" }],
    hidden: 0,
    total: 3,
    more: false,
    page: 0,
    capped: false,
    ...over,
  };
}

function problem(status: number, type: string, errors: { location: string; message: string }[]) {
  return Response.json(
    { type, title: "x", status, detail: "x", errors },
    { status, headers: { "Content-Type": "application/problem+json" } },
  );
}

const where = [
  { location: "registry.host", message: "ghcr.io" },
  { location: "registry.repo", message: "acme/app" },
];

beforeEach(() => {
  sent = [];
  detailsSent = [];
  FakeObserver.all = [];
  vi.stubGlobal("IntersectionObserver", FakeObserver);
  detailsAnswer = (body) => Response.json({ items: body.tags.map((tag) => detail(tag)) });
  me = makeMe({ instanceAdmin: true, envs: { e1: "viewer" } });
  answer = () => Response.json(list());
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const url = new URL(req.url);
      if (url.pathname === "/api/me") return Response.json(me);
      if (url.pathname === "/api/registries/tags") {
        const entry: Sent = { path: url.pathname, query: Object.fromEntries(url.searchParams) };
        sent.push(entry);
        return answer(entry);
      }
      if (url.pathname === "/api/registries/tags/details") {
        const body = (await req.json()) as DetailsBody;
        detailsSent.push(body);
        return detailsAnswer(body);
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

function mount(node: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>{node}</TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

const rows = () => screen.getAllByRole("row").slice(1);

describe("TagBrowser", () => {
  it("shows skeleton rows while the list loads, then the tag names in mono", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);

    expect(screen.getAllByRole("row")).toHaveLength(8);
    expect(screen.queryByText("v1.10.0")).toBeNull();

    await waitFor(() => expect(sent).toHaveLength(1));
    release(Response.json(list()));
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(rows().map((r) => within(r).getAllByRole("cell")[1]?.textContent)).toEqual([
      "v1.10.0",
      "v1.9.2",
      "latest",
    ]);
    expect(screen.getByText("v1.10.0").className).toContain("font-mono");
    expect(screen.getByText("Showing 3 tags")).toBeTruthy();
    expect(sent).toEqual([{ path: "/api/registries/tags", query: { ref: "ghcr.io/acme/app" } }]);
  });

  it("names the credential used, linked to it", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText(/Using credential/)).toBeTruthy());
    const line = screen.getByText(/Using credential/);
    expect(line.textContent).toBe("Using credential “ghcr-acme” (Instance)");
    expect(within(line).getByRole("link", { name: "ghcr-acme" }).getAttribute("href")).toBe(
      "/registries?credential=c1",
    );
  });

  it("says Anonymous when no credential was used, and an environment's name for its credential", async () => {
    answer = () => Response.json(list({ credential: null }));
    mount(<TagBrowser repo="nginx" variant="full" />);
    await waitFor(() => expect(screen.getByText("Anonymous")).toBeTruthy());
    cleanup();
    client.clear();

    answer = () =>
      Response.json(
        list({
          credential: {
            ...instanceCred,
            scope: { kind: "environment", envId: "e1", envName: "Lab" },
          },
        }),
      );
    mount(<TagBrowser repo="nginx" variant="full" envId="e1" />);
    await waitFor(() =>
      expect(screen.getByText(/Using credential/).textContent).toBe(
        "Using credential “ghcr-acme” (Lab)",
      ),
    );
    expect(sent.at(-1)?.query).toEqual({ ref: "nginx", envId: "e1" });
  });

  it("shows No tags with a way to try again when the repository has none", async () => {
    answer = () => Response.json(list({ tags: [], total: 0 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("No tags")).toBeTruthy());
    expect(screen.getByText("acme/app has no tags on ghcr.io.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(sent).toHaveLength(2));
  });

  it("truncates a long tag name and keeps it whole in the DOM", async () => {
    const long = `v${"9".repeat(120)}`;
    answer = () => Response.json(list({ tags: [{ name: long }], total: 1 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText(long)).toBeTruthy());
    expect(screen.getByText(long).className).toContain("truncate");
  });

  it("renders a tag name that looks like markup as text", async () => {
    answer = () =>
      Response.json(list({ tags: [{ name: "<img src=x onerror=alert(1)>" }], total: 1 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeTruthy());
    expect(document.querySelector("img")).toBeNull();
  });

  it("selects a row in the picker with a click or Enter, and marks it selected", async () => {
    const onSelect = vi.fn();
    mount(
      <TagBrowser repo="ghcr.io/acme/app" variant="picker" onSelect={onSelect} selected="v1.9.2" />,
    );
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    const [first, second] = rows();
    expect(first?.getAttribute("aria-selected")).toBe("false");
    expect(second?.getAttribute("aria-selected")).toBe("true");
    fireEvent.click(first as HTMLElement);
    fireEvent.keyDown(rows()[2] as HTMLElement, { key: "Enter" });
    expect(onSelect.mock.calls).toEqual([["v1.10.0"], ["latest"]]);
  });
});

describe("TagBrowser errors", () => {
  it("login_refused names the credential and links to test it", async () => {
    answer = () =>
      problem(502, "login_refused", [
        ...where,
        { location: "credential.id", message: "c1" },
        { location: "credential.name", message: "ghcr-acme" },
        { location: "credential.scopeKind", message: "instance" },
        { location: "credential.scopeName", message: "" },
      ]);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "Bayplan couldn't sign in to ghcr.io with credential “ghcr-acme” (Instance). Test it.",
    );
    expect(within(alert).getByRole("link", { name: "Test credential" }).getAttribute("href")).toBe(
      "/registries?credential=c1",
    );
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("needs_login with no credential links to add one, for someone who may", async () => {
    answer = () => problem(502, "needs_login", where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "ghcr.io needs a login to list tags, and no stored credential matches acme/app. Add one.",
    );
    expect(
      (await within(alert).findByRole("link", { name: "Add credential" })).getAttribute("href"),
    ).toBe("/registries?credential=new");
  });

  it("needs_login offers no Add credential link to someone who cannot add one", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    answer = () => problem(502, "needs_login", where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    await waitFor(() => expect(alert.textContent).toContain("needs a login"));
    expect(within(alert).queryByRole("link")).toBeNull();
  });

  it("repository_not_found names the repository and the registry, with nothing to retry", async () => {
    answer = () => problem(404, "repository_not_found", where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("No repository named acme/app on ghcr.io.");
    expect(within(alert).queryByRole("button")).toBeNull();
  });

  it.each([
    ["registry_unreachable", "Couldn't reach ghcr.io. Check the address and try again."],
    ["rate_limited", "ghcr.io is limiting requests. Wait a moment and try again."],
  ])("%s says so and Retry asks again", async (type, message) => {
    answer = () => problem(502, type, where);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(message);
    answer = () => Response.json(list());
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("never shows what the registry said", async () => {
    answer = () =>
      Response.json(
        {
          type: "registry_unreachable",
          title: "x",
          status: 502,
          detail: "SECRET-REGISTRY-TEXT",
          errors: where,
        },
        { status: 502, headers: { "Content-Type": "application/problem+json" } },
      );
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await screen.findByRole("alert");
    expect(document.body.textContent).not.toContain("SECRET-REGISTRY-TEXT");
  });

  it("falls back to a generic message when the server cannot be reached", async () => {
    answer = () => {
      throw new TypeError("network down");
    };
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Couldn't load the tags.");
    expect(within(alert).getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

describe("TagBrowser filter, hide switch and paging", () => {
  const hideSwitch = () =>
    screen.getByRole("switch", { name: "Hide signature, attestation and cache tags" });
  const filterBox = () => screen.getByRole("searchbox", { name: "Filter tags" });

  it("has a filter and a Hide switch that is on, with how many it hides", async () => {
    answer = () => Response.json(list({ hidden: 4 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(filterBox().getAttribute("placeholder")).toBe("Filter by name");
    expect(hideSwitch().getAttribute("aria-checked")).toBe("true");
    expect(screen.getByText("4 hidden")).toBeTruthy();
  });

  it("shows no hidden count when nothing is hidden or the switch is off", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByText(/hidden$/)).toBeNull();
    cleanup();
    client.clear();

    answer = () => Response.json(list({ hidden: 4 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("4 hidden")).toBeTruthy());
    fireEvent.click(hideSwitch());
    await waitFor(() => expect(hideSwitch().getAttribute("aria-checked")).toBe("false"));
    expect(screen.queryByText("4 hidden")).toBeNull();
  });

  it("asks again for the filter text and for the switch turned off", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app" });

    fireEvent.change(filterBox(), { target: { value: "alpine" } });
    await waitFor(() =>
      expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app", q: "alpine" }),
    );

    fireEvent.click(hideSwitch());
    await waitFor(() =>
      expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app", q: "alpine", hide: "false" }),
    );
  });

  it("loads 100 more, and says That's every tag. at the end", async () => {
    answer = (req) =>
      req.query.page === "1"
        ? Response.json(list({ tags: [{ name: "v0.1.0" }], total: 4, more: false, page: 1 }))
        : Response.json(list({ total: 4, more: true, page: 0 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByText("That's every tag.")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Load 100 more" }));
    await waitFor(() => expect(screen.getByText("v0.1.0")).toBeTruthy());
    expect(sent.at(-1)?.query).toEqual({ ref: "ghcr.io/acme/app", page: "1" });
    expect(screen.getByText("That's every tag.")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Load 100 more" })).toBeNull();
    expect(screen.getByText("Showing 4 tags")).toBeTruthy();
    expect(rows().map((r) => r.textContent)).toEqual(["v1.10.0", "v1.9.2", "latest", "v0.1.0"]);
  });

  it("shows no match for the filter, with a button that clears it", async () => {
    answer = (req) =>
      req.query.q ? Response.json(list({ tags: [], total: 0 })) : Response.json(list());
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    fireEvent.change(filterBox(), { target: { value: "zzz" } });
    await waitFor(() => expect(screen.getByText("No tags match “zzz”.")).toBeTruthy());
    // The filter box has a clear button of the same name; this is the one in the empty state.
    const empty = screen
      .getByText("No tags match “zzz”.")
      .closest("[data-slot='empty']") as HTMLElement;
    fireEvent.click(within(empty).getByRole("button", { name: "Clear filter" }));
    // The unfiltered list was read a moment ago, so it comes back without asking again.
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(sent.map((s) => s.query.q)).toEqual([undefined, "zzz"]);
  });

  it("says only signature and attestation tags are left, with a button that shows them", async () => {
    answer = (req) =>
      req.query.hide === "false"
        ? Response.json(list({ tags: [{ name: "app.sig" }], total: 1, hidden: 1 }))
        : Response.json(list({ tags: [], total: 0, hidden: 3 }));
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() =>
      expect(screen.getByText("Only signature and attestation tags are left.")).toBeTruthy(),
    );
    fireEvent.click(screen.getByRole("button", { name: "Show them" }));
    await waitFor(() => expect(screen.getByText("app.sig")).toBeTruthy());
    expect(hideSwitch().getAttribute("aria-checked")).toBe("false");
  });

  it("keeps the filter and the switch in the address when the page owns them", async () => {
    const onSearchChange = vi.fn();
    mount(
      <BrowsePage
        search={{ ref: "ghcr.io/acme/app", q: "alp", hide: 0 }}
        onSearchChange={onSearchChange}
      />,
    );
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]?.query).toEqual({ ref: "ghcr.io/acme/app", q: "alp", hide: "false" });
    await waitFor(() => expect(hideSwitch().getAttribute("aria-checked")).toBe("false"));
    expect((filterBox() as HTMLInputElement).value).toBe("alp");

    fireEvent.click(hideSwitch());
    expect(onSearchChange).toHaveBeenLastCalledWith({ hide: undefined });
    fireEvent.change(filterBox(), { target: { value: "alpine" } });
    await waitFor(() => expect(onSearchChange).toHaveBeenLastCalledWith({ q: "alpine" }));
  });
});

describe("BrowsePage", () => {
  it("shows the breadcrumb, the repository as the title and the tag browser", async () => {
    mount(<BrowsePage search={{ ref: "ghcr.io/acme/app" }} />);
    await screen.findByRole("navigation", { name: "Breadcrumb" });
    expect(screen.getByRole("navigation", { name: "Breadcrumb" }).textContent).toBe(
      "RegistriesBrowse",
    );
    expect(
      within(screen.getByRole("navigation", { name: "Breadcrumb" }))
        .getByRole("link", { name: "Registries" })
        .getAttribute("href"),
    ).toBe("/registries");
    expect(screen.getByRole("heading", { level: 1 }).textContent).toBe("ghcr.io/acme/app");
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
  });

  it("asks nothing and offers the way back when there is no repository in the address", async () => {
    mount(<BrowsePage search={{ ref: "" }} />);
    expect(await screen.findByText("No repository to browse")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to Registries" })).toBeTruthy();
    expect(sent).toHaveLength(0);
  });

  it("shows no permission to someone who reads no environment", async () => {
    me = makeMe({});
    mount(<BrowsePage search={{ ref: "ghcr.io/acme/app" }} />);
    expect(await screen.findByText("You don't have permission to view this page")).toBeTruthy();
    expect(sent).toHaveLength(0);
    expect(screen.queryByRole("heading", { level: 1, name: "ghcr.io/acme/app" })).toBeNull();
  });
});

// The details of the rows on screen (plan 05-19, IMG-03, D-20, D-21, D-23).

const cellsOf = (row: HTMLElement) => within(row).getAllByRole("cell");
const rowOf = (tag: string) =>
  screen.getAllByRole("row").find((r) => r.getAttribute("data-tag") === tag) as HTMLElement;

describe("TagBrowser details", () => {
  it("shows Loading… without moving anything until a row is on screen, then asks for the rows on screen only", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(detailsSent).toHaveLength(0);

    const [, , digest, created, platforms] = cellsOf(rowOf("v1.10.0")) as HTMLElement[];
    expect(digest?.textContent).toBe("Loading…");
    expect(digest?.getAttribute("aria-busy")).toBe("true");
    expect(created?.textContent).toBe("Loading…");
    expect(created?.getAttribute("aria-busy")).toBe("true");
    expect(platforms?.textContent).toBe("—");

    scrollTo(["v1.10.0", "latest"]);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    expect(detailsSent[0]).toEqual({ ref: "ghcr.io/acme/app", tags: ["v1.10.0", "latest"] });
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).not.toBe("Loading…"));
    // The row that was never on screen is still waiting.
    expect(cellsOf(rowOf("v1.9.2"))[2]?.textContent).toBe("Loading…");
  });

  it("fills a row with a short mono digest whose tooltip holds the whole one, the age, and a chip", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" envId="e1" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"));
    expect(detailsSent[0]).toEqual({ ref: "ghcr.io/acme/app", envId: "e1", tags: ["v1.10.0"] });

    const cells = cellsOf(rowOf("v1.10.0"));
    const short = within(cells[2] as HTMLElement).getByText("sha256:a1a1a1a1a1a1");
    expect(short.className).toContain("font-mono");
    fireEvent.focus(short);
    expect((await screen.findByRole("tooltip")).textContent).toBe(D1);
    expect(cells[3]?.textContent).toBe("1 h ago");
    expect(cells[3]?.getAttribute("aria-busy")).not.toBe("true");
    expect(within(cells[4] as HTMLElement).getByText("amd64")).toBeTruthy();
  });

  it("sends at most one request at a time, 25 tags in each", async () => {
    const names = Array.from({ length: 60 }, (_, i) => `v${i}`);
    answer = () => Response.json(list({ tags: names.map((name) => ({ name })), total: 60 }));
    const releases: (() => void)[] = [];
    detailsAnswer = (body) =>
      new Promise<Response>((resolve) =>
        releases.push(() =>
          resolve(Response.json({ items: body.tags.map((tag) => detail(tag)) })),
        ),
      );
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v0")).toBeTruthy());
    scrollTo(names);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    expect(detailsSent[0]?.tags).toEqual(names.slice(0, 25));
    // Nothing more is sent while the first is out.
    await new Promise((r) => setTimeout(r, 50));
    expect(detailsSent).toHaveLength(1);
    await act(async () => releases[0]?.());
    await waitFor(() => expect(detailsSent).toHaveLength(2));
    expect(detailsSent[1]?.tags).toEqual(names.slice(25, 50));
    await act(async () => releases[1]?.());
    await waitFor(() => expect(detailsSent).toHaveLength(3));
    expect(detailsSent[2]?.tags).toEqual(names.slice(50));
    await act(async () => releases[2]?.());
    await waitFor(() => expect(cellsOf(rowOf("v59"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"));
  });

  it("keeps what rows resolved when they leave the screen, and asks again only for rows still waiting", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"));
    scrollTo(["v1.10.0"], false);
    scrollTo(["v1.10.0", "v1.9.2"]);
    await waitFor(() => expect(detailsSent).toHaveLength(2));
    expect(detailsSent[1]?.tags).toEqual(["v1.9.2"]);
    expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1");
  });

  it("leaves a row on Loading… when its request failed and asks again when it is scrolled to again", async () => {
    detailsAnswer = () => Response.json({}, { status: 503 });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    await new Promise((r) => setTimeout(r, 50));
    expect(detailsSent).toHaveLength(1); // no loop
    expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("Loading…");
    detailsAnswer = (body) => Response.json({ items: body.tags.map((tag) => detail(tag)) });
    scrollTo(["v1.10.0"], false);
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"));
  });

  it("reorders the batch that arrived by date, newest first, once, in the places it occupied", async () => {
    const ago = (days: number) => new Date(Date.now() - days * 86_400_000).toISOString();
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) =>
          detail(tag, { created: tag === "latest" ? ago(1) : tag === "v1.10.0" ? ago(3) : ago(10) }),
        ),
      });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    const order = () => rows().map((r) => r.getAttribute("data-tag"));
    expect(order()).toEqual(["v1.10.0", "v1.9.2", "latest"]);

    // Only the last two are on screen: they swap inside their own places.
    scrollTo(["v1.9.2", "latest"]);
    await waitFor(() => expect(order()).toEqual(["v1.10.0", "latest", "v1.9.2"]));
    // The first one arrives later as a batch of its own and has no one to be ordered against.
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(detailsSent).toHaveLength(2));
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[3]?.textContent).toBe("3 d ago"));
    expect(order()).toEqual(["v1.10.0", "latest", "v1.9.2"]);
  });

  it("does not reorder rows whose dates the list already carried", async () => {
    const ago = (days: number) => new Date(Date.now() - days * 86_400_000).toISOString();
    answer = () =>
      Response.json(
        list({
          tags: [
            { name: "new", digest: D1, created: ago(1) },
            { name: "old", digest: D2, created: ago(9) },
          ],
          total: 2,
        }),
      );
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) => detail(tag, { created: tag === "new" ? ago(30) : ago(1) })),
      });
    mount(<TagBrowser repo="docker.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("new")).toBeTruthy());
    scrollTo(["new", "old"]);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    await waitFor(() => expect(cellsOf(rowOf("new"))[3]?.textContent).toBe("1 d ago"));
    expect(rows().map((r) => r.getAttribute("data-tag"))).toEqual(["new", "old"]);
  });

  it("shows a dash with a tooltip for a date that could not be read, and keeps the rest of the row", async () => {
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) => detail(tag, { created: null, code: "date_unavailable" })),
      });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"));
    const created = cellsOf(rowOf("v1.10.0"))[3] as HTMLElement;
    expect(created.textContent).toBe("—");
    expect(created.getAttribute("aria-busy")).not.toBe("true");
    fireEvent.focus(within(created).getByText("—"));
    expect((await screen.findByRole("tooltip")).textContent).toBe("Couldn't read this tag's date.");
    expect(within(cellsOf(rowOf("v1.10.0"))[4] as HTMLElement).getByText("amd64")).toBeTruthy();
  });

  it("shows a tag the registry no longer has as dashes", async () => {
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) =>
          detail(tag, { digest: "", created: null, platforms: [], code: "tag_not_found" }),
        ),
      });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("—"));
    expect(cellsOf(rowOf("v1.10.0"))[3]?.textContent).toBe("—");
    expect(cellsOf(rowOf("v1.10.0"))[4]?.textContent).toBe("—");
  });

  it("uses the digest, date and platforms the list carried without waiting for details", async () => {
    answer = () =>
      Response.json(
        list({
          tags: [
            {
              name: "1.0",
              digest: D2,
              created: new Date(Date.now() - 7_200_000).toISOString(),
              platforms: [platform("arm64", ARM)],
            },
          ],
          total: 1,
        }),
      );
    mount(<TagBrowser repo="docker.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("1.0")).toBeTruthy());
    const cells = cellsOf(rowOf("1.0"));
    expect(cells[2]?.textContent).toBe("sha256:b2b2b2b2b2b2");
    expect(cells[3]?.textContent).toBe("2 h ago");
    expect(within(cells[4] as HTMLElement).getByText("arm64")).toBeTruthy();
  });
});

describe("TagBrowser platforms", () => {
  const many = [
    platform("amd64", AMD),
    platform("arm64", ARM),
    platform("arm", `sha256:${"e5".repeat(32)}`, { variant: "v7" }),
    platform("386", `sha256:${"f6".repeat(32)}`),
    platform("ppc64le", `sha256:${"07".repeat(32)}`),
    platform("amd64", `sha256:${"18".repeat(32)}`, { os: "windows", size: 0 }),
  ];

  async function openWith(platforms: unknown[]) {
    detailsAnswer = (body) =>
      Response.json({ items: body.tags.map((tag) => detail(tag, { platforms })) });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"));
    return cellsOf(rowOf("v1.10.0"))[4] as HTMLElement;
  }

  it("shows one chip and no expander for a single platform", async () => {
    const cell = await openWith([platform("amd64", AMD)]);
    expect(cell.textContent).toBe("amd64");
    expect(screen.queryByRole("button", { name: "Show platforms for v1.10.0" })).toBeNull();
  });

  it("shows the first four chips then +n with a tooltip naming the rest", async () => {
    const cell = await openWith(many);
    expect(cell.textContent).toBe("amd64arm64arm/v7386+2");
    fireEvent.focus(within(cell).getByText("+2"));
    expect((await screen.findByRole("tooltip")).textContent).toBe("ppc64le, windows/amd64");
  });

  it("opens a table of each platform's digest and size under the row, and closes it", async () => {
    await openWith(many);
    const toggle = screen.getByRole("button", { name: "Show platforms for v1.10.0" });
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    const region = document.getElementById(toggle.getAttribute("aria-controls") ?? "") as HTMLElement;
    expect(region).toBeTruthy();
    const table = within(region).getByRole("table");
    expect(within(table).getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Platform",
      "Digest",
      "Size",
    ]);
    expect(within(table).getByText(`Index digest ${D1}`)).toBeTruthy();
    const body = within(table).getAllByRole("row").slice(1);
    expect(body).toHaveLength(6);
    expect(cellsOf(body[0] as HTMLElement).map((c) => c.textContent)).toEqual(["linux/amd64", AMD, "5 MiB"]);
    expect(cellsOf(body[2] as HTMLElement)[0]?.textContent).toBe("linux/arm/v7");
    expect(cellsOf(body[5] as HTMLElement).map((c) => c.textContent)).toEqual(["windows/amd64", `sha256:${"18".repeat(32)}`, "—"]);
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(document.getElementById(toggle.getAttribute("aria-controls") ?? "")).toBeNull();
  });

  it("renders a platform string that looks like markup as text", async () => {
    const cell = await openWith([platform("<b>x</b>", AMD), platform("arm64", ARM)]);
    expect(cell.textContent).toContain("<b>x</b>");
    expect(document.querySelector("b")).toBeNull();
  });

  it("shows Tag, Created and Platforms only in the picker", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="picker" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Tag",
      "Created",
      "Platforms",
    ]);
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(cellsOf(rowOf("v1.10.0"))[1]?.textContent).toBe("1 h ago"));
    expect(within(cellsOf(rowOf("v1.10.0"))[2] as HTMLElement).getByText("amd64")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Show platforms/ })).toBeNull();
  });

  it("shows Tag, Digest, Created and Platforms in the full table, after an unlabelled expander column", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "",
      "Tag",
      "Digest",
      "Created",
      "Platforms",
    ]);
  });
});
