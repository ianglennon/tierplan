// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { createRef } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { setCsrfToken } from "@/api/client";
import type { StackRevision } from "@/api/stack-history";
import { TooltipProvider } from "@/components/ui/tooltip";
import { RevisionDetail } from "@/features/stack-history/revision-detail";
import "@/i18n";
import { TestRouter } from "@/test-router";

const ENV = "e1";
const DIGEST_A = `sha256:${"ab12".repeat(16)}`;
const DIGEST_B = `sha256:${"cd34".repeat(16)}`;

type Json = Record<string, unknown>;

let client: QueryClient;
let detailStatus: number;
let detailType: string;
let detail: Json;
let detailCalls: number;

function item(seq: number, over: Partial<StackRevision> = {}): StackRevision {
  return {
    seq,
    origin: "deploy",
    restoredFrom: 0,
    outcome: "good",
    createdBy: "ada",
    createdAt: "2026-10-20T10:00:00Z",
    failedService: "",
    code: "",
    reason: "",
    deployed: false,
    images: [],
    pinned: 0,
    adoptedFrom: "",
    ...over,
  };
}

// What the server answers for revision 1 while revision 3 is deployed: a secret variable and a secret file
// that stay hidden, a plain variable, two pinned images and a diff of one replaced line.
function oldRevision(): Json {
  return {
    item: item(1),
    deployedSeq: 3,
    rendered: "services:\n  web:\n    image: web:1\n    environment:\n      TOKEN: <hidden>\n",
    files: [
      { path: "compose.yaml", secret: false },
      { path: "tok.txt", secret: true },
    ],
    vars: [
      { name: "PLAIN", value: "visible", secret: false },
      { name: "TOKEN", value: "<hidden>", secret: true },
    ],
    pins: [
      { service: "db", image: `db:1@${DIGEST_B}`, digest: DIGEST_B },
      { service: "web", image: `web:1@${DIGEST_A}`, digest: DIGEST_A },
    ],
    yaml: {
      changedLines: 2,
      hunks: [
        {
          oldStart: 1,
          oldLines: 3,
          newStart: 1,
          newLines: 3,
          lines: [
            { kind: "context", old: 1, new: 1, text: "services:" },
            { kind: "removed", old: 2, new: 0, text: "  web: image: web:3" },
            { kind: "added", old: 0, new: 2, text: "  web: image: web:1" },
          ],
        },
      ],
      lines: ["services:", "  web: image: web:1"],
    },
  };
}

const NO_DIFF = { changedLines: 0, hunks: [], lines: [] };

beforeEach(() => {
  detailStatus = 200;
  detailType = "";
  detailCalls = 0;
  detail = oldRevision();
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path.startsWith(`/api/env/${ENV}/stacks/s1/revisions/`)) {
        detailCalls++;
        if (detailStatus !== 200) {
          return Response.json(
            { type: detailType, title: "Problem", status: detailStatus, detail: "x" },
            { status: detailStatus },
          );
        }
        return Response.json(detail);
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

function mount(seq = 1, over: Partial<StackRevision> = {}, retention?: number) {
  return render(
    <QueryClientProvider client={client}>
      <TestRouter>
        <TooltipProvider>
          <RevisionDetail
            envId={ENV}
            stackId="s1"
            item={item(seq, over)}
            stack={undefined}
            headingRef={createRef<HTMLHeadingElement>()}
            retention={retention}
          />
        </TooltipProvider>
      </TestRouter>
    </QueryClientProvider>,
  );
}

async function openContents() {
  const tab = await screen.findByRole("tab", { name: "Contents" });
  fireEvent.mouseDown(tab);
  fireEvent.click(tab);
  return tab;
}

describe("the Contents tab", () => {
  it("lists Files, Variables and Pinned images in native tables with the roll back helper", async () => {
    mount();
    await openContents();

    const files = await screen.findByRole("table", { name: "Files of revision #1" });
    expect(
      within(files)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["File", "Type"]);
    const vars = screen.getByRole("table", { name: "Variables of revision #1" });
    expect(
      within(vars)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Name", "Value", "Secret"]);
    const pins = screen.getByRole("table", { name: "Images pinned by revision #1" });
    expect(
      within(pins)
        .getAllByRole("columnheader")
        .map((h) => h.textContent),
    ).toEqual(["Service", "Image", "Digest"]);
    expect(
      screen.getByText(
        "A roll back restores all of this exactly, including the pinned images. It never looks anything up in a registry.",
      ),
    ).toBeTruthy();
  });

  it("shows a file as Plain or with a Secret badge, and a secret file's content as Hidden", async () => {
    mount();
    await openContents();

    const files = await screen.findByRole("table", { name: "Files of revision #1" });
    const plain = within(files).getByText("compose.yaml").closest("tr") as HTMLElement;
    expect(within(plain).getByText("Plain")).toBeTruthy();
    const secret = within(files).getByText("tok.txt").closest("tr") as HTMLElement;
    expect(within(secret).getByText("Secret")).toBeTruthy();
    expect(within(secret).getByText("Hidden")).toBeTruthy();
    expect(within(plain).queryByText("Hidden")).toBeNull();
  });

  it("shows a variable's value through MaskedValue when secret, and a plain value as it is", async () => {
    mount();
    await openContents();

    const vars = await screen.findByRole("table", { name: "Variables of revision #1" });
    const plain = within(vars).getByText("PLAIN").closest("tr") as HTMLElement;
    expect(within(plain).getByText("visible")).toBeTruthy();
    expect(within(plain).getByText("No")).toBeTruthy();
    const secret = within(vars).getByText("TOKEN").closest("tr") as HTMLElement;
    expect(within(secret).getByText("••••••••")).toBeTruthy();
    expect(within(secret).getByText("Yes")).toBeTruthy();
    expect(within(secret).queryByText("<hidden>")).toBeNull();
  });

  it("lists the pinned images by service with the digest as a chip", async () => {
    mount();
    await openContents();

    const pins = await screen.findByRole("table", { name: "Images pinned by revision #1" });
    const web = within(pins).getByText("web").closest("tr") as HTMLElement;
    expect(within(web).getByText(`web:1@${DIGEST_A}`)).toBeTruthy();
    expect(within(web).getByRole("button", { name: `Pinned to ${DIGEST_A}` })).toBeTruthy();
    expect(within(pins).getAllByRole("row")).toHaveLength(3);
  });

  it("counts one file, two variables and a plural of images with the right word", async () => {
    detail = {
      ...oldRevision(),
      files: [{ path: "compose.yaml", secret: false }],
      pins: [{ service: "web", image: `web:1@${DIGEST_A}`, digest: DIGEST_A }],
    };
    mount();
    await openContents();

    expect(await screen.findByText("1 file")).toBeTruthy();
    expect(screen.getByText("2 variables")).toBeTruthy();
    expect(screen.getByText("1 image")).toBeTruthy();
  });

  it("says so for a revision with no variables and no pinned images, and still lists compose.yaml", async () => {
    detail = {
      ...oldRevision(),
      vars: [],
      pins: [],
    };
    mount();
    await openContents();

    expect(await screen.findByText("This revision has no variables.")).toBeTruthy();
    expect(screen.getByText("This revision pinned no images.")).toBeTruthy();
    expect(screen.getByText("compose.yaml")).toBeTruthy();
  });

  it("has no Reveal control on any tab", async () => {
    mount();
    for (const name of ["YAML diff", "Rendered", "Contents"]) {
      const tab = await screen.findByRole("tab", { name });
      fireEvent.mouseDown(tab);
      fireEvent.click(tab);
      await waitFor(() => expect(tab.getAttribute("aria-selected")).toBe("true"));
      expect(screen.queryByRole("button", { name: /reveal|show value/i })).toBeNull();
      expect(screen.queryByText(/reveal/i)).toBeNull();
    }
  });
});

describe("the YAML diff tab", () => {
  it("draws the difference from the deployed revision under its caption", async () => {
    mount();

    const table = await screen.findByRole("table", {
      name: "Rendered YAML, revision #1 against #3",
    });
    expect(within(table).getByText("web: image: web:1")).toBeTruthy();
    expect(within(table).getByText("web: image: web:3")).toBeTruthy();
  });

  it("says a revision with the same content as the deployed one has no changes", async () => {
    detail = { ...oldRevision(), item: item(2), yaml: NO_DIFF };
    mount(2);

    expect(await screen.findByText("No changes")).toBeTruthy();
    expect(
      screen.getByText("Revision #2 has the same content as the deployed revision."),
    ).toBeTruthy();
    expect(screen.queryByRole("table")).toBeNull();
  });

  it("says the deployed revision itself has no changes", async () => {
    detail = { ...oldRevision(), item: item(3, { deployed: true }), deployedSeq: 3, yaml: NO_DIFF };
    mount(3, { deployed: true });

    expect(await screen.findByText("No changes")).toBeTruthy();
    expect(screen.getByText("This is the deployed revision.", { selector: "p" })).toBeTruthy();
  });

  it("says there is nothing to compare when no revision is deployed or none was rendered", async () => {
    detail = { ...oldRevision(), deployedSeq: 0, yaml: NO_DIFF };
    mount();
    expect(await screen.findByText("Nothing to compare with")).toBeTruthy();
    cleanup();

    detail = { ...oldRevision(), rendered: "", yaml: NO_DIFF };
    mount();
    expect(await screen.findByText("No rendering stored")).toBeTruthy();
  });
});

describe("the Rendered tab", () => {
  it("offers the stored rendering read only with Copy and Download", async () => {
    mount();
    const tab = await screen.findByRole("tab", { name: "Rendered" });
    fireEvent.mouseDown(tab);
    fireEvent.click(tab);

    expect(await screen.findByText("Revision #1, rendered")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download" })).toBeTruthy();
    expect(
      await screen.findByRole("textbox", { name: "Rendered stack, revision 1, read only" }),
    ).toBeTruthy();
  });
});

describe("loading, errors and a gone revision", () => {
  it("shows skeleton tabs and a skeleton surface while the revision loads", async () => {
    mount();

    const loading = screen.getByTestId("revision-loading");
    expect(loading.getAttribute("aria-busy")).toBe("true");
    expect(screen.queryByRole("tab")).toBeNull();
    await screen.findByRole("tab", { name: "Contents" });
    expect(screen.queryByTestId("revision-loading")).toBeNull();
  });

  it("shows an inline error with Retry when a tab cannot load, and loads it again on Retry", async () => {
    detailStatus = 500;
    detailType = "internal";
    mount();

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("Couldn't load this revision's YAML.")).toBeTruthy();
    const before = detailCalls;
    detailStatus = 200;
    fireEvent.click(within(alert).getByRole("button", { name: "Retry" }));

    await screen.findByRole("table", { name: "Rendered YAML, revision #1 against #3" });
    expect(detailCalls).toBeGreaterThan(before);
  });

  it("says it is the contents that could not load on the Contents tab", async () => {
    detailStatus = 500;
    detailType = "internal";
    mount();
    await openContents();

    expect(await screen.findByText("Couldn't load this revision's contents.")).toBeTruthy();
  });

  it("turns the card into 'That revision is gone' when the server says the revision was deleted", async () => {
    detailStatus = 404;
    detailType = "revision_not_found";
    mount(1, {}, 20);

    expect(await screen.findByText("That revision is gone")).toBeTruthy();
    expect(
      screen.getByText(
        "Bayplan keeps the last 20 revisions of each stack, and this one was deleted.",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Back to history" }).getAttribute("href")).toBe(
      "/env/e1/stacks/s1/history",
    );
    expect(screen.queryByRole("tab")).toBeNull();
  });

  it("names the default of 20 revisions when the instance's limit is not known", async () => {
    detailStatus = 404;
    detailType = "revision_not_found";
    mount();

    expect(
      await screen.findByText(
        "Bayplan keeps the last 20 revisions of each stack, and this one was deleted.",
      ),
    ).toBeTruthy();
  });
});
