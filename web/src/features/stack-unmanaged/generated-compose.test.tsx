// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { setCsrfToken } from "@/api/client";
import { GeneratedCompose } from "@/features/stack-unmanaged/generated-compose";
import "@/i18n";
import { editorText } from "@/test-editor";

// The Generated compose tab of an unmanaged stack (plan 08-11, STK-12, D-09, D-12, D-47): what Bayplan would use
// if the stack were adopted, rebuilt from the running services, read only.

const ENV = "e1";
const PATH = `/api/env/${ENV}/unmanaged-stacks/legacy/compose`;
const YAML = "services:\n  app:\n    image: nginx:1.27\n    environment:\n      DB_PASSWORD: <hidden>\n";

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
      if (new URL(req.url).pathname === PATH) {
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

function mount() {
  render(
    <QueryClientProvider client={client}>
      <GeneratedCompose envId={ENV} name="legacy" />
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
        "What Bayplan would use if you adopt this stack, rebuilt from the running services. It is a preview: nothing is saved. Hidden values show as <hidden>.",
      ),
    ).toBeTruthy();
  });

  it("says how many secrets and configs are referenced as external, and nothing when there are none", async () => {
    answer = () => ok({ externalSecrets: 2, externalConfigs: 1 });
    mount();
    expect(
      await screen.findByText(
        "2 secrets and 1 config are referenced as external. Bayplan never reads their values.",
      ),
    ).toBeTruthy();
    cleanup();

    answer = () => ok();
    mount();
    await screen.findByRole("textbox", { name: "Generated compose, read only" });
    expect(screen.queryByText(/referenced as external/)).toBeNull();
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
        "Bayplan couldn't rebuild legacy: services.app.image: the rebuilt file is not valid",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect((screen.getByRole("button", { name: "Copy" }) as HTMLButtonElement).disabled).toBe(true);

    answer = () => ok();
    fireEvent.click(screen.getByRole("button", { name: "Retry rebuild" }));

    expect(await screen.findByRole("textbox", { name: "Generated compose, read only" })).toBeTruthy();
    expect(composeRequests).toBe(2);
  });
});
