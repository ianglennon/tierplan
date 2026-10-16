// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { toast } from "sonner";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { Toaster } from "@/components/ui/sonner";
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

// One date for every default detail: the browser puts a batch newest first, and equal dates
// keep the order the list gave.
const AN_HOUR_AGO = new Date(Date.now() - 3_600_000).toISOString();

function detail(tag: string, over: Record<string, unknown> = {}) {
  return {
    tag,
    digest: D1,
    created: AN_HOUR_AGO,
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
      if (url.pathname === "/api/environments")
        return Response.json({
          items: [
            { id: "e1", name: "Lab host", envType: "swarm", enabled: true },
            { id: "e2", name: "Edge", envType: "standalone", enabled: true },
          ],
          total: 2,
        });
      const envPath = /^\/api\/env\/([^/]+)\/images\/tags(\/details)?$/.exec(url.pathname);
      if (envPath?.[2]) {
        const body = (await req.json()) as DetailsBody;
        detailsSent.push({ ...body, envId: envPath[1] });
        return detailsAnswer({ ...body, envId: envPath[1] });
      }
      if (url.pathname === "/api/registries/tags" || envPath) {
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
  // A toast one test raised (the failed copy) would otherwise still be on screen in the next
  // test's Toaster and be found there as its role="alert".
  toast.dismiss();
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  client.clear();
});

function mount(node: React.ReactNode) {
  render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          {node}
          <Toaster />
        </TooltipProvider>
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
    mount(<TagBrowser repo="nginx" variant="full" credentialEnvId="e1" />);
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

  it("repository_unavailable says the repository can't be read, with no Test credential link and no Retry", async () => {
    answer = () =>
      problem(404, "repository_unavailable", [
        ...where,
        { location: "credential.id", message: "c1" },
        { location: "credential.name", message: "ghcr-acme" },
        { location: "credential.scopeKind", message: "instance" },
        { location: "credential.scopeName", message: "" },
      ]);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain(
      "ghcr.io has no repository acme/app that credential “ghcr-acme” (Instance) can read. It may not exist, or the account may not have access.",
    );
    expect(alert.textContent).not.toContain("sign in");
    expect(within(alert).queryByRole("link")).toBeNull();
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

  it("words Bayplan's own 429 as too many requests, not as the registry limiting them", async () => {
    answer = () => problem(429, "rate_limited", []);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toContain("Too many tag requests. Wait a minute and try again.");
    expect(alert.textContent).not.toContain("is limiting requests");
    answer = () => Response.json(list());
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
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
    expect(rows().map((r) => r.getAttribute("data-tag"))).toEqual([
      "v1.10.0",
      "v1.9.2",
      "latest",
      "v0.1.0",
    ]);
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
  it("opens the pull form from a row with the reference and an Environment select of the environments the person can pull on", async () => {
    me = makeMe({ envs: { e1: "operator", e2: "viewer" } });
    mount(<BrowsePage search={{ ref: "ghcr.io/acme/app" }} />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    const trigger = within(rowOf("v1.10.0")).getByRole("button", { name: "Actions for v1.10.0" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    fireEvent.click(await screen.findByRole("menuitem", { name: "Pull to an environment…" }));
    const dialog = await screen.findByRole("dialog", { name: "Pull an image" });
    expect((within(dialog).getByLabelText("Image") as HTMLInputElement).value).toBe(
      "ghcr.io/acme/app:v1.10.0",
    );
    fireEvent.click(within(dialog).getByRole("combobox", { name: "Environment" }));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual(["Lab host"]);
  });

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
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" credentialEnvId="e1" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
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
        releases.push(() => resolve(Response.json({ items: body.tags.map((tag) => detail(tag)) }))),
      );
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v0")).toBeTruthy());
    scrollTo(names);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    expect(detailsSent[0]?.tags).toEqual(names.slice(0, 25));
    // Nothing more is sent while the first is out. The request is only sent after the hook has
    // marked itself busy in the same effect, so once the effects of this render have flushed a
    // second request would already be in detailsSent.
    await act(async () => {});
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
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
    scrollTo(["v1.10.0"], false);
    scrollTo(["v1.10.0", "v1.9.2"]);
    await waitFor(() => expect(detailsSent).toHaveLength(2));
    expect(detailsSent[1]?.tags).toEqual(["v1.9.2"]);
    expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1");
  });

  it("leaves a row on Loading… when its request failed and asks again when it is scrolled to again", async () => {
    // Only the request that includes v1.10.0 fails; the later one for "latest" is the signal
    // that the failure has been recorded (requests go one at a time), so "no loop" is asserted
    // after it instead of after a guessed delay.
    detailsAnswer = (body) =>
      body.tags.includes("v1.10.0")
        ? Response.json({}, { status: 503 })
        : Response.json({ items: body.tags.map((tag) => detail(tag)) });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    scrollTo(["v1.10.0", "latest"]);
    await waitFor(() => expect(detailsSent).toHaveLength(2));
    expect(detailsSent[1]?.tags).toEqual(["latest"]); // v1.10.0 was not asked for again: no loop
    await waitFor(() =>
      expect(cellsOf(rowOf("latest"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
    expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("Loading…");
    detailsAnswer = (body) => Response.json({ items: body.tags.map((tag) => detail(tag)) });
    scrollTo(["v1.10.0"], false);
    scrollTo(["v1.10.0"]);
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
  });

  it("reorders the batch that arrived by date, newest first, once, in the places it occupied", async () => {
    const ago = (days: number) =>
      new Date(Date.now() - days * 86_400_000 - 3_600_000).toISOString();
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) =>
          detail(tag, {
            created: tag === "latest" ? ago(1) : tag === "v1.10.0" ? ago(3) : ago(10),
          }),
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
    const ago = (days: number) =>
      new Date(Date.now() - days * 86_400_000 - 3_600_000).toISOString();
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
    await waitFor(() => expect(cellsOf(rowOf("old"))[3]?.textContent).toBe("1 d ago"));
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
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
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
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
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
    const region = document.getElementById(
      toggle.getAttribute("aria-controls") ?? "",
    ) as HTMLElement;
    expect(region).toBeTruthy();
    const table = within(region).getByRole("table");
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Platform", "Digest", "Size"]);
    expect(within(table).getByText(`Index digest ${D1}`)).toBeTruthy();
    const body = within(table).getAllByRole("row").slice(1);
    expect(body).toHaveLength(6);
    expect(cellsOf(body[0] as HTMLElement).map((c) => c.textContent)).toEqual([
      "linux/amd64",
      AMD,
      "5 MiB",
    ]);
    expect(cellsOf(body[2] as HTMLElement)[0]?.textContent).toBe("linux/arm/v7");
    expect(cellsOf(body[5] as HTMLElement).map((c) => c.textContent)).toEqual([
      "windows/amd64",
      `sha256:${"18".repeat(32)}`,
      "—",
    ]);
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

  it("shows Tag, Digest, Created, Platforms and Running in on the Registries page, after an unlabelled expander column", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "",
      "Tag",
      "Digest",
      "Created",
      "Platforms",
      "Running in",
      "Actions",
    ]);
  });

  it("has Running here in place of Running in inside an environment, where the markers are that environment's own", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" envId="e1" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "",
      "Tag",
      "Digest",
      "Created",
      "Platforms",
      "Running here",
      "Actions",
    ]);
    expect(sent.at(-1)).toEqual({
      path: "/api/env/e1/images/tags",
      query: { ref: "ghcr.io/acme/app" },
    });
  });
});

describe("TagBrowser Running in", () => {
  const running = (names: string[]) => names.map((n, i) => ({ envId: `e${i + 1}`, envName: n }));

  async function openWith(runningIn: { envId: string; envName: string }[]) {
    detailsAnswer = (body) =>
      Response.json({ items: body.tags.map((tag) => detail(tag, { runningIn })) });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(cellsOf(rowOf("v1.10.0"))[5]?.textContent).toBe("Loading…");
    expect(cellsOf(rowOf("v1.10.0"))[5]?.getAttribute("aria-busy")).toBe("true");
    scrollTo(["v1.10.0"]);
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
    return cellsOf(rowOf("v1.10.0"))[5] as HTMLElement;
  }

  it("shows a dash when no environment runs the tag", async () => {
    const cell = await openWith([]);
    expect(cell.textContent).toBe("—");
  });

  it("shows up to two environment names as links to them", async () => {
    const cell = await openWith(running(["Lab", "Edge"]));
    const links = within(cell).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["Lab", "Edge"]);
    expect(links.map((l) => l.getAttribute("href"))).toEqual(["/env/e1", "/env/e2"]);
    expect(within(cell).queryByRole("button")).toBeNull();
  });

  it("collapses the rest into +n, which opens a popover with the others as links", async () => {
    const cell = await openWith(running(["Lab", "Edge", "Prod", "Dev"]));
    expect(
      within(cell)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual(["Lab", "Edge"]);
    const more = within(cell).getByRole("button", { name: "Show 2 more environments" });
    expect(more.textContent).toBe("+2");
    fireEvent.click(more);
    const popover = await screen.findByRole("dialog");
    expect(
      within(popover)
        .getAllByRole("link")
        .map((l) => l.textContent),
    ).toEqual(["Prod", "Dev"]);
    expect(
      within(popover)
        .getAllByRole("link")
        .map((l) => l.getAttribute("href")),
    ).toEqual(["/env/e3", "/env/e4"]);
  });

  it("draws an environment name that looks like markup as text", async () => {
    const cell = await openWith(running(["<b>lab</b>"]));
    expect(within(cell).getByRole("link").textContent).toBe("<b>lab</b>");
    expect(document.querySelector("b")).toBeNull();
  });
});

describe("TagBrowser Running here", () => {
  const here = {
    containers: [
      { id: "c1", name: "web", nodeId: "n1", node: "pi-1" },
      { id: "c2", name: "api.1.abc", nodeId: "n2", node: "pi-2" },
    ],
    services: [{ id: "s1", name: "api", nodes: ["pi-2"] }],
  };

  async function openEnv(runningHere?: unknown) {
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) =>
          detail(tag, tag === "v1.10.0" && runningHere ? { runningHere } : {}),
        ),
      });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" envId="e1" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(cellsOf(rowOf("v1.10.0"))[5]?.textContent).toBe("Loading…");
    scrollTo(["v1.10.0", "v1.9.2"]);
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
    return cellsOf(rowOf("v1.10.0"))[5] as HTMLElement;
  }

  it("names the credential above the list, as the Registries page does", async () => {
    await openEnv();
    expect(screen.getByText(/Using credential/).textContent).toBe(
      "Using credential “ghcr-acme” (Instance)",
    );
  });

  it("marks a tag that runs here with a badge, and leaves a tag that does not run here unmarked", async () => {
    const cell = await openEnv(here);
    expect(within(cell).getByRole("button", { name: "Running here" })).toBeTruthy();
    expect(cellsOf(rowOf("v1.9.2"))[5]?.textContent).toBe("");
  });

  it("opens a popover listing the containers as links and the services with their nodes", async () => {
    const cell = await openEnv(here);
    fireEvent.click(within(cell).getByRole("button", { name: "Running here" }));
    const popover = await screen.findByRole("dialog");
    expect(within(popover).getByText("2 containers, 1 service")).toBeTruthy();
    const links = within(popover).getAllByRole("link");
    expect(links.map((l) => l.textContent)).toEqual(["web", "api.1.abc"]);
    expect(links[0]?.getAttribute("href")).toBe("/env/e1/containers/c1?node=n1");
    expect(within(popover).getByText("pi-1")).toBeTruthy();
    const service = within(popover).getByText("api");
    expect(service.closest("li")?.textContent).toBe("apipi-2");
    expect(service.closest("a")).toBeNull();
  });

  it("opens the popover from the keyboard as well", async () => {
    const cell = await openEnv(here);
    const trigger = within(cell).getByRole("button", { name: "Running here" });
    trigger.focus();
    fireEvent.keyDown(trigger, { key: "Enter" });
    fireEvent.click(trigger);
    expect(await screen.findByRole("dialog")).toBeTruthy();
  });

  it("reads one container as singular", async () => {
    const cell = await openEnv({ containers: [here.containers[0]], services: [] });
    fireEvent.click(within(cell).getByRole("button", { name: "Running here" }));
    const popover = await screen.findByRole("dialog");
    expect(within(popover).getByText("1 container, 0 services")).toBeTruthy();
  });
});

// The missing-platform flag, the row actions and the picker (plan 05-20, IMG-03, D-19,
// D-23, D-24).

describe("TagBrowser missing platforms", () => {
  async function open(props: { envId?: string }, missing: string[]) {
    detailsAnswer = (body) =>
      Response.json({ items: body.tags.map((tag) => detail(tag, { missing })) });
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" {...props} />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
    return cellsOf(rowOf("v1.10.0"))[4] as HTMLElement;
  }

  it("flags a tag that lacks an architecture the environment's nodes run, beside its chips", async () => {
    const cell = await open({ envId: "e1" }, ["arm64"]);
    expect(within(cell).getByText("amd64")).toBeTruthy();
    const flag = within(cell).getByText("No arm64");
    expect(flag.closest("[data-slot='missing-platform']")?.querySelector("svg")).toBeTruthy();
  });

  it("explains the flag in a tooltip that names the environment", async () => {
    const cell = await open({ envId: "e1" }, ["arm64"]);
    const flag = within(cell).getByText("No arm64");
    fireEvent.focus(flag.closest("[data-slot='missing-platform']") as HTMLElement);
    expect(
      await screen.findByRole("tooltip", {
        name: "Nodes in Lab host run arm64, and this tag doesn't include it.",
      }),
    ).toBeTruthy();
  });

  it("flags every missing architecture", async () => {
    const cell = await open({ envId: "e1" }, ["arm/v7", "arm64"]);
    expect(within(cell).getByText("No arm/v7")).toBeTruthy();
    expect(within(cell).getByText("No arm64")).toBeTruthy();
  });

  it("shows no flag when the tag covers every architecture", async () => {
    const cell = await open({ envId: "e1" }, []);
    expect(within(cell).queryByText(/^No /)).toBeNull();
  });

  it("never flags on the Registries page, which has no single environment to compare with", async () => {
    const cell = await open({}, ["arm64"]);
    expect(within(cell).queryByText(/^No /)).toBeNull();
  });
});

describe("TagBrowser row actions", () => {
  const REF = "ghcr.io/acme/app";

  async function menu(props: { envId?: string; onPull?: (ref: string) => void } = {}) {
    mount(<TagBrowser repo={REF} variant="full" {...props} />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0"]);
    await waitFor(() =>
      expect(cellsOf(rowOf("v1.10.0"))[2]?.textContent).toBe("sha256:a1a1a1a1a1a1"),
    );
    const trigger = within(rowOf("v1.10.0")).getByRole("button", { name: "Actions for v1.10.0" });
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false });
    fireEvent.click(trigger);
    return screen.findByRole("menu");
  }

  it("offers Pull to this environment, Copy repo:tag and Copy repo@sha256:… in an environment", async () => {
    me = makeMe({ envs: { e1: "operator" } });
    const m = await menu({ envId: "e1", onPull: vi.fn() });
    expect(
      within(m)
        .getAllByRole("menuitem")
        .map((i) => i.textContent),
    ).toEqual(["Pull to this environment", "Copy repo:tag", "Copy repo@sha256:…"]);
  });

  it("offers Pull to an environment… in place of the first on the Registries page", async () => {
    me = makeMe({ envs: { e1: "operator" } });
    const m = await menu({ onPull: vi.fn() });
    expect(within(m).getAllByRole("menuitem")[0]?.textContent).toBe("Pull to an environment…");
  });

  it("asks the host to pull the full reference", async () => {
    me = makeMe({ envs: { e1: "operator" } });
    const onPull = vi.fn();
    const m = await menu({ envId: "e1", onPull });
    fireEvent.click(within(m).getByRole("menuitem", { name: "Pull to this environment" }));
    expect(onPull).toHaveBeenCalledWith(`${REF}:v1.10.0`);
  });

  it("is unavailable with Needs Operator for a Viewer, and pulls nothing", async () => {
    me = makeMe({ envs: { e1: "viewer" } });
    const onPull = vi.fn();
    const m = await menu({ envId: "e1", onPull });
    const item = within(m).getByRole("menuitem", { name: "Pull to this environment" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(item.getAttribute("aria-describedby")).toBeTruthy();
    fireEvent.click(item);
    expect(onPull).not.toHaveBeenCalled();
    expect(await screen.findAllByText("Needs Operator")).not.toHaveLength(0);
  });

  it("copies repo:tag and says so", async () => {
    const write = vi.fn(async () => undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: write } });
    const m = await menu({ envId: "e1", onPull: vi.fn() });
    fireEvent.click(within(m).getByRole("menuitem", { name: "Copy repo:tag" }));
    await waitFor(() => expect(write).toHaveBeenCalledWith(`${REF}:v1.10.0`));
    expect(await screen.findByText(`Copied ${REF}:v1.10.0.`)).toBeTruthy();
  });

  it("copies repo@sha256:… with the whole digest", async () => {
    const write = vi.fn(async () => undefined);
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: write } });
    const m = await menu({ envId: "e1", onPull: vi.fn() });
    fireEvent.click(within(m).getByRole("menuitem", { name: "Copy repo@sha256:…" }));
    await waitFor(() => expect(write).toHaveBeenCalledWith(`${REF}@${D1}`));
    expect(await screen.findByText(`Copied ${REF}@${D1}.`)).toBeTruthy();
  });

  it("says to copy by hand when the copy fails everywhere", async () => {
    const write = vi.fn(async () => {
      throw new Error("denied");
    });
    vi.stubGlobal("navigator", { ...navigator, clipboard: { writeText: write } });
    const m = await menu({ envId: "e1", onPull: vi.fn() });
    fireEvent.click(within(m).getByRole("menuitem", { name: "Copy repo:tag" }));
    expect(
      await screen.findByText("Copy failed. Select the text and copy it manually."),
    ).toBeTruthy();
  });

  it("has no Pull item when the host gives no way to pull", async () => {
    const m = await menu({ envId: "e1" });
    expect(
      within(m)
        .getAllByRole("menuitem")
        .map((i) => i.textContent),
    ).toEqual(["Copy repo:tag", "Copy repo@sha256:…"]);
  });
});

describe("TagBrowser picker", () => {
  async function pick(props: { envId?: string; selected?: string } = {}) {
    const onSelect = vi.fn();
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="picker" onSelect={onSelect} {...props} />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0", "v1.9.2", "latest"]);
    // The rows settle once the details have come: they are put newest first when they do.
    await waitFor(() => expect(screen.queryAllByText("Loading…")).toHaveLength(0));
    return onSelect;
  }
  const pickerRows = () => screen.getAllByRole("row").slice(1);

  it("lists Tag, Created and Platforms only, with no actions", async () => {
    await pick();
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Tag",
      "Created",
      "Platforms",
    ]);
    expect(screen.queryByRole("button", { name: /Actions for/ })).toBeNull();
  });

  it("marks the chosen row aria-selected, with a check", async () => {
    await pick({ selected: "v1.9.2" });
    expect(pickerRows().map((r) => r.getAttribute("aria-selected"))).toEqual([
      "false",
      "true",
      "false",
    ]);
    expect(within(rowOf("v1.9.2")).getByText("Selected")).toBeTruthy();
  });

  it("selects a tag by a click and by Enter", async () => {
    const onSelect = await pick();
    fireEvent.click(rowOf("v1.9.2"));
    expect(onSelect).toHaveBeenLastCalledWith("v1.9.2");
    fireEvent.keyDown(rowOf("latest"), { key: "Enter" });
    expect(onSelect).toHaveBeenLastCalledWith("latest");
  });

  it("moves between rows with ArrowDown, ArrowUp, Home and End", async () => {
    await pick();
    const rows = pickerRows();
    rows[0]?.focus();
    fireEvent.keyDown(rows[0] as HTMLElement, { key: "ArrowDown" });
    expect(document.activeElement).toBe(rows[1]);
    fireEvent.keyDown(rows[1] as HTMLElement, { key: "End" });
    expect(document.activeElement).toBe(rows[2]);
    fireEvent.keyDown(rows[2] as HTMLElement, { key: "ArrowUp" });
    expect(document.activeElement).toBe(rows[1]);
    fireEvent.keyDown(rows[1] as HTMLElement, { key: "Home" });
    expect(document.activeElement).toBe(rows[0]);
    fireEvent.keyDown(rows[0] as HTMLElement, { key: "ArrowUp" });
    expect(document.activeElement).toBe(rows[0]);
  });

  it("shows Running here as a small dot with the words and no popover", async () => {
    detailsAnswer = (body) =>
      Response.json({
        items: body.tags.map((tag) =>
          detail(
            tag,
            tag === "v1.9.2"
              ? {
                  runningHere: {
                    containers: [{ id: "c", name: "web", nodeId: "n", node: "pi" }],
                    services: [],
                  },
                }
              : {},
          ),
        ),
      });
    await pick({ envId: "e1" });
    await waitFor(() => expect(within(rowOf("v1.9.2")).getByText("Running here")).toBeTruthy());
    expect(within(rowOf("v1.9.2")).queryByRole("button")).toBeNull();
    expect(within(rowOf("v1.10.0")).queryByText("Running here")).toBeNull();
  });

  it("is at most 320px tall with its own scroll", async () => {
    await pick();
    expect(screen.getByRole("grid").closest(".max-h-80")).toBeTruthy();
  });

  it("is a grid of rows with one tab stop, which follows focus (WR-C04)", async () => {
    await pick();
    expect(screen.queryByRole("table")).toBeNull();
    const rows = pickerRows();
    expect(rows.map((r) => r.getAttribute("tabindex"))).toEqual(["0", "-1", "-1"]);
    rows[1]?.focus();
    await waitFor(() =>
      expect(pickerRows().map((r) => r.getAttribute("tabindex"))).toEqual(["-1", "0", "-1"]),
    );
    fireEvent.keyDown(rows[1] as HTMLElement, { key: "End" });
    await waitFor(() =>
      expect(pickerRows().map((r) => r.getAttribute("tabindex"))).toEqual(["-1", "-1", "0"]),
    );
  });

  it("starts the tab stop on the chosen row", async () => {
    await pick({ selected: "v1.9.2" });
    expect(pickerRows().map((r) => r.getAttribute("tabindex"))).toEqual(["-1", "0", "-1"]);
  });

  it("keeps the full list a plain table whose rows are not focusable or selectable", async () => {
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    expect(screen.queryByRole("grid")).toBeNull();
    for (const row of screen.getAllByRole("row")) {
      expect(row.hasAttribute("aria-selected")).toBe(false);
      expect(row.hasAttribute("tabindex")).toBe(false);
    }
  });
});

describe("TagBrowser details that cannot be read (WR-B01, WR-B05)", () => {
  it("never asks about a name the server would refuse, and reads it as a tag with no manifest", async () => {
    answer = () =>
      Response.json(
        list({
          tags: [{ name: "v1.10.0" }, { name: "../etc" }, { name: "a?b" }, { name: "latest" }],
        }),
      );
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0", "../etc", "a?b", "latest"]);
    await waitFor(() => expect(detailsSent).toHaveLength(1));
    expect(detailsSent[0]?.tags).toEqual(["v1.10.0", "latest"]);
    await waitFor(() => expect(screen.queryAllByText("Loading…")).toHaveLength(0));
  });

  it("says so when the server turned the request for details away as too frequent", async () => {
    detailsAnswer = () => problem(429, "rate_limited", []);
    mount(<TagBrowser repo="ghcr.io/acme/app" variant="full" />);
    await waitFor(() => expect(screen.getByText("v1.10.0")).toBeTruthy());
    scrollTo(["v1.10.0", "v1.9.2", "latest"]);
    expect(
      await screen.findByText(/Too many requests, so some details haven't loaded/),
    ).toBeTruthy();
  });
});
