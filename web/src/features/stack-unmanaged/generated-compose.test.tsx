// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { GeneratedCompose } from "@/features/stack-unmanaged/generated-compose";
import "@/i18n";
import { editorText } from "@/test-editor";

// The Generated compose tab of an unmanaged stack (plan 08-11, STK-12, D-09, D-12, D-47): what TierPlan would use
// if the stack were adopted, rebuilt from the running services, read only.

const ENV = "e1";
const PATH = `/api/env/${ENV}/unmanaged-stacks/legacy/compose`;
const SHOP_PATH = `/api/env/${ENV}/unmanaged-stacks/shop/compose`;
const YAML =
  "services:\n  app:\n    image: nginx:1.27\n    environment:\n      DB_PASSWORD: <hidden>\n";

type Answer = {
  yaml: string;
  notes: { service: string; code: string; message: string }[];
  externalSecrets: number;
  externalConfigs: number;
};

let answer: () => Response | Promise<Response>;
// The compose requests the page made, by path: the SPA also sends other requests of its own.
let composeRequests: number;
let client: QueryClient;

const ok = (over: Partial<Answer> = {}) =>
  Response.json({ yaml: YAML, notes: [], externalSecrets: 0, externalConfigs: 0, ...over });

beforeEach(() => {
  composeRequests = 0;
  answer = () => ok();
  client = new QueryClient({ defaultOptions: { queries: { retry: false, retryDelay: 0 } } });
  setCsrfToken(null);
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      const path = new URL(req.url).pathname;
      if (path === PATH || path === SHOP_PATH) {
        composeRequests++;
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
  client.clear();
});

function mount(name = "legacy") {
  render(
    <QueryClientProvider client={client}>
      <GeneratedCompose envId={ENV} name={name} />
    </QueryClientProvider>,
  );
}

describe("the Generated compose tab", () => {
  it("shows a skeleton and Rebuilding… until the server answers, then the file in a read-only textbox", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount();
    expect(await screen.findByRole("status", { name: "Rebuilding…" })).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();

    release(ok());
    const box = await screen.findByRole("textbox", { name: "Generated compose, read only" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(editorText()).toBe(YAML);
    expect(screen.queryByRole("status", { name: "Rebuilding…" })).toBeNull();
    expect(screen.getByRole("heading", { name: "Generated compose" })).toBeTruthy();
    expect(
      screen.getByText(
        "What TierPlan would use if you adopt this stack, rebuilt from the running services. It is a preview: nothing is saved. Hidden values show as <hidden>.",
      ),
    ).toBeTruthy();
  });

  it("shows the rebuilt file of a docker compose project the same way, with the project's name in the failure", async () => {
    const compose =
      "services:\n  web:\n    image: nginx:1.27\n    environment:\n      DB_PASSWORD: <hidden>\n    depends_on:\n      - db\n    volumes:\n      - data:/data\n  db:\n    image: postgres:17\nvolumes:\n  data: {}\n";
    answer = () => ok({ yaml: compose });
    mount("shop");
    await screen.findByRole("textbox", { name: "Generated compose, read only" });
    expect(editorText()).toBe(compose);
    expect(screen.queryByRole("heading", { name: "Notes" })).toBeNull();
    cleanup();
    client.clear();

    answer = () =>
      Response.json(
        {
          type: "rebuild_failed",
          title: "Can't rebuild",
          status: 422,
          detail: "no container of shop is running",
        },
        { status: 422 },
      );
    mount("shop");
    expect(
      await screen.findByText("TierPlan couldn't rebuild shop: no container of shop is running"),
    ).toBeTruthy();
  });

  it("says how many secrets and configs are referenced as external, and nothing when there are none", async () => {
    answer = () => ok({ externalSecrets: 2, externalConfigs: 1 });
    mount();
    expect(
      await screen.findByText(
        "2 secrets and 1 config are referenced as external. TierPlan never reads their values.",
      ),
    ).toBeTruthy();
    cleanup();
    // The first answer is in the cache under the same key.
    client.clear();

    answer = () => ok();
    mount();
    await screen.findByRole("textbox", { name: "Generated compose, read only" });
    expect(screen.queryByText(/referenced as external/)).toBeNull();
  });

  it("lists the notes of the rebuild under the file, and no Notes heading when there are none", async () => {
    answer = () =>
      ok({
        notes: [
          {
            service: "app",
            code: "image_unpinned",
            message:
              "app runs nginx:1.27, which has no digest, so it is pinned to the reference as it is.",
          },
          {
            service: "",
            code: "network_not_overlay",
            message:
              "The network legacy_lan uses the macvlan driver, not overlay, so the file uses it as an external network. TierPlan doesn't create it.",
          },
        ],
      });
    mount();
    const box = await screen.findByRole("textbox", { name: "Generated compose, read only" });
    const notes = screen.getByRole("heading", { name: "Notes" }).closest("section") as HTMLElement;
    const items = within(notes).getAllByRole("listitem");
    expect(items.map((li) => li.textContent)).toEqual([
      "app runs nginx:1.27, which has no digest, so it is pinned to the reference as it is.",
      "The network legacy_lan uses the macvlan driver, not overlay, so the file uses it as an external network. TierPlan doesn't create it.",
    ]);
    // The notes come after the file.
    expect(box.compareDocumentPosition(notes) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    cleanup();
    client.clear();

    answer = () => ok();
    mount();
    await screen.findByRole("textbox", { name: "Generated compose, read only" });
    expect(screen.queryByRole("heading", { name: "Notes" })).toBeNull();
  });

  it("words the notes of a docker compose project by their code, one line each, with the server's sentence for a code it doesn't know", async () => {
    answer = () =>
      ok({
        notes: [
          { service: "api", code: "image_local", message: "the server's own wording" },
          {
            service: "shop_data",
            code: "object_external_hash",
            message: "the server's own wording",
          },
          { service: "", code: "a_new_code", message: "A sentence from a newer server." },
        ],
      });
    mount("shop");
    await screen.findByRole("textbox", { name: "Generated compose, read only" });
    const notes = screen.getByRole("heading", { name: "Notes" }).closest("section") as HTMLElement;
    expect(
      within(notes)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual([
      "api runs an image built on this host. It is pinned to its image ID; TierPlan can't look it up in a registry.",
      "shop_data is kept external: TierPlan couldn't match its settings, so a deploy leaves it as it is.",
      "A sentence from a newer server.",
    ]);
  });

  it("copies the text through the clipboard", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mount();
    await screen.findByRole("textbox", { name: "Generated compose, read only" });

    fireEvent.click(screen.getByRole("button", { name: "Copy" }));

    await waitFor(() => expect(writeText).toHaveBeenCalledWith(YAML));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();
  });

  it("downloads {name}.generated.yaml from a Blob and makes no request", async () => {
    const created: Blob[] = [];
    const createObjectURL = vi.fn((blob: Blob) => {
      created.push(blob);
      return "blob:generated";
    });
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL, revokeObjectURL }));
    const downloads: { name: string; href: string }[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloads.push({ name: this.download, href: this.href });
    });
    mount();
    await screen.findByRole("textbox", { name: "Generated compose, read only" });
    const requests = composeRequests;

    fireEvent.click(screen.getByRole("button", { name: "Download" }));

    expect(downloads).toEqual([{ name: "legacy.generated.yaml", href: "blob:generated" }]);
    expect(await created[0]?.text()).toBe(YAML);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:generated");
    expect(composeRequests).toBe(requests);
    click.mockRestore();
  });
});

describe("a stack that cannot be rebuilt", () => {
  it("says so with the reason, and Retry rebuild asks again", async () => {
    answer = () =>
      Response.json(
        {
          type: "rebuild_failed",
          title: "Unprocessable Entity",
          status: 422,
          detail: "services.app.image: the rebuilt file is not valid",
        },
        { status: 422 },
      );
    mount();
    expect(await screen.findByText("Can't generate the compose file")).toBeTruthy();
    expect(
      screen.getByText(
        "TierPlan couldn't rebuild legacy: services.app.image: the rebuilt file is not valid",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect((screen.getByRole("button", { name: "Copy" }) as HTMLButtonElement).disabled).toBe(true);

    answer = () => ok();
    fireEvent.click(screen.getByRole("button", { name: "Retry rebuild" }));

    expect(
      await screen.findByRole("textbox", { name: "Generated compose, read only" }),
    ).toBeTruthy();
    expect(composeRequests).toBe(2);
  });
});
