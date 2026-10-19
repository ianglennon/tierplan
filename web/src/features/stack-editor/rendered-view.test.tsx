// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { RenderedView, renderedTiming } from "@/features/stack-editor/rendered-view";
import {
  emptyWorkingCopy,
  type WorkingCopy,
  workingCopyReducer,
} from "@/features/stack-editor/working-copy";
import "@/i18n";
import { editorText } from "@/test-editor";

// The Rendered pane (plan 07-13, STK-04, D-18): the stack as it will be deployed, read only. The
// clock is fake and the server is a stubbed fetch, so the 1.5 s pause is the one in the contract.

const COMPOSE = "services:\n  web:\n    image: nginx:1.27-alpine\n";
const RENDERED = `name: shop
services:
  web:
    image: nginx:1.27-alpine # will be pinned when you deploy
`;

function copyWith(compose: string, vars: { name: string; value: string; secret?: boolean }[] = []) {
  return workingCopyReducer(emptyWorkingCopy, {
    type: "load",
    draft: {
      isDraft: true,
      version: 1,
      savedBy: "ada",
      savedAt: "2026-10-19T10:00:00Z",
      baseSeq: 0,
      files: [{ path: "compose.yaml", content: compose, secret: false }],
      vars: vars.map((v) => ({ name: v.name, value: v.value, secret: v.secret ?? false })),
    },
  });
}

let sent: { files: unknown; vars: unknown }[];
let answer: () => Response | Promise<Response>;
const onShowProblems = vi.fn();

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  sent = [];
  onShowProblems.mockReset();
  answer = () => Response.json({ name: "shop", yaml: RENDERED });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/env/e1/stacks/s1/render") {
        sent.push(await req.clone().json());
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
  Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });
});

function mount(copy: WorkingCopy, saveCount = 0) {
  const view = render(
    <RenderedView
      envId="e1"
      stackId="s1"
      copy={copy}
      saveCount={saveCount}
      onShowProblems={onShowProblems}
    />,
  );
  return {
    ...view,
    update: (next: WorkingCopy, count = saveCount) =>
      view.rerender(
        <RenderedView
          envId="e1"
          stackId="s1"
          copy={next}
          saveCount={count}
          onShowProblems={onShowProblems}
        />,
      ),
  };
}

async function flush() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

describe("the Rendered pane", () => {
  it("shows a skeleton and Rendering… until the server answers, then the stack in a read-only textbox", async () => {
    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    mount(copyWith(COMPOSE));
    await flush();
    expect(screen.getByRole("status", { name: "Rendering…" })).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();

    release(Response.json({ name: "shop", yaml: RENDERED }));
    await flush();
    const box = screen.getByRole("textbox", { name: "Rendered stack, read only" });
    expect(box.getAttribute("aria-readonly")).toBe("true");
    expect(editorText()).toBe(RENDERED);
    expect(screen.queryByText("Rendering…")).toBeNull();
    expect(screen.getByText("Rendered stack")).toBeTruthy();
    expect(
      screen.getByText(
        "The compose file after variables are filled in and images are pinned, as it will be deployed. Hidden values show as <hidden>. Bayplan never changes your compose file.",
      ),
    ).toBeTruthy();
  });

  it("sends the working copy as the save shape, with a hidden secret as the keep marker", async () => {
    mount(copyWith(COMPOSE, [{ name: "DB_PASS", value: "<hidden>", secret: true }]));
    await flush();
    expect(sent).toEqual([
      {
        files: [{ path: "compose.yaml", content: COMPOSE, secret: false }],
        vars: [{ name: "DB_PASS", value: "<hidden>", secret: true }],
      },
    ]);
  });

  it("renders again 1.5 s after the last edit, restarting the wait on each one", async () => {
    const view = mount(copyWith(COMPOSE));
    await flush();
    expect(sent).toHaveLength(1);

    view.update(copyWith(`${COMPOSE}# a\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs - 100);
    });
    view.update(copyWith(`${COMPOSE}# ab\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs - 100);
    });
    expect(sent).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100);
    });
    expect(sent).toHaveLength(2);
    const files = (sent[1]?.files ?? []) as { content: string }[];
    expect(files[0]?.content).toBe(`${COMPOSE}# ab\n`);
  });

  it("renders again at once after a save, and not a second time for the save's own copy", async () => {
    const view = mount(copyWith(COMPOSE));
    await flush();
    expect(sent).toHaveLength(1);

    view.update(copyWith(COMPOSE), 1);
    await flush();
    expect(sent).toHaveLength(2);
    // The copy the save leaves has the same content, so the pause that follows it asks for nothing.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs * 2);
    });
    expect(sent).toHaveLength(2);
  });

  it("keeps the last stack on screen, marked as updating, while the next one is on its way", async () => {
    const view = mount(copyWith(COMPOSE));
    await flush();
    screen.getByRole("textbox", { name: "Rendered stack, read only" });

    let release: (r: Response) => void = () => {};
    answer = () => new Promise<Response>((resolve) => (release = resolve));
    view.update(copyWith(`${COMPOSE}# more\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs);
    });
    expect(screen.getByText("Updating…")).toBeTruthy();
    expect(editorText()).toBe(RENDERED);

    release(Response.json({ name: "shop", yaml: "name: shop\n" }));
    await flush();
    expect(editorText()).toBe("name: shop\n");
    expect(screen.queryByText("Updating…")).toBeNull();
  });
});

describe("a stack that cannot be rendered", () => {
  it("says so, with Show problems that hands over to the page, when the compose file has errors", async () => {
    answer = () =>
      Response.json(
        { type: "stack_invalid", title: "x", status: 422, detail: "d" },
        { status: 422 },
      );
    mount(copyWith("services:\n  web:\n    imag: x\n"));
    await flush();
    expect(screen.getByText("Can't render the stack yet")).toBeTruthy();
    expect(
      screen.getByText(
        "Fix the errors in the compose file first. The rendered stack shows here once it is valid.",
      ),
    ).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect((screen.getByRole("button", { name: "Copy" }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(screen.getByRole("button", { name: "Show problems" }));
    expect(onShowProblems).toHaveBeenCalledTimes(1);
  });

  it("replaces the stack that was shown when the next edit makes the file invalid, and shows it again when it is fixed", async () => {
    const view = mount(copyWith(COMPOSE));
    await flush();
    screen.getByRole("textbox", { name: "Rendered stack, read only" });

    answer = () =>
      Response.json(
        { type: "stack_invalid", title: "x", status: 422, detail: "d" },
        { status: 422 },
      );
    view.update(copyWith("services:\n  web:\n    imag: x\n"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs);
    });
    expect(screen.getByText("Can't render the stack yet")).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();

    answer = () => Response.json({ name: "shop", yaml: RENDERED });
    view.update(copyWith(COMPOSE));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs);
    });
    expect(screen.getByRole("textbox", { name: "Rendered stack, read only" })).toBeTruthy();
  });

  it("says the server could not render it, with Retry, and retries on request only", async () => {
    answer = () => new Response(null, { status: 500 });
    mount(copyWith(COMPOSE));
    await flush();
    expect(screen.getByText("Couldn't render the stack")).toBeTruthy();
    expect(screen.getByText("Check the connection and try again.")).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs * 3);
    });
    expect(sent).toHaveLength(1);

    answer = () => Response.json({ name: "shop", yaml: RENDERED });
    fireEvent.click(screen.getByRole("button", { name: "Retry" }));
    await flush();
    expect(screen.getByRole("textbox", { name: "Rendered stack, read only" })).toBeTruthy();
    expect(sent).toHaveLength(2);
  });

  it("says when the checks are busy", async () => {
    answer = () =>
      Response.json({ type: "check_busy", title: "x", status: 429, detail: "d" }, { status: 429 });
    mount(copyWith(COMPOSE));
    await flush();
    expect(screen.getByText("Too many checks are running. Try again in a moment.")).toBeTruthy();
  });

  it("keeps the last stack, and says it is the last, when a refresh fails", async () => {
    const view = mount(copyWith(COMPOSE));
    await flush();
    screen.getByRole("textbox", { name: "Rendered stack, read only" });

    answer = () => new Response(null, { status: 500 });
    view.update(copyWith(`${COMPOSE}# more\n`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.debounceMs);
    });
    expect(
      screen.getByText("Couldn't update the rendered stack. This is the last one that rendered."),
    ).toBeTruthy();
    expect(editorText()).toBe(RENDERED);
  });
});

describe("Copy and Download", () => {
  it("copies the rendered text, and says Copied for two seconds", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    mount(copyWith(COMPOSE));
    await flush();
    screen.getByRole("textbox", { name: "Rendered stack, read only" });

    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await flush();
    expect(writeText).toHaveBeenCalledWith(RENDERED);
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(renderedTiming.copiedMs);
    });
    expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
  });

  it("downloads {name}.rendered.yaml from a Blob and makes no request", async () => {
    const created: Blob[] = [];
    const createObjectURL = vi.fn((blob: Blob) => {
      created.push(blob);
      return "blob:rendered";
    });
    const revokeObjectURL = vi.fn();
    vi.stubGlobal("URL", Object.assign(URL, { createObjectURL, revokeObjectURL }));
    const downloads: { name: string; href: string }[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      downloads.push({ name: this.download, href: this.href });
    });
    mount(copyWith(COMPOSE));
    await flush();
    screen.getByRole("textbox", { name: "Rendered stack, read only" });
    const requests = sent.length;

    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(downloads).toEqual([{ name: "shop.rendered.yaml", href: "blob:rendered" }]);
    expect(await created[0]?.text()).toBe(RENDERED);
    expect(revokeObjectURL).toHaveBeenCalledWith("blob:rendered");
    expect(sent).toHaveLength(requests);
    click.mockRestore();
  });

  it("does not let a name make a path of the downloaded file", async () => {
    answer = () => Response.json({ name: "../etc/x", yaml: RENDERED });
    vi.stubGlobal(
      "URL",
      Object.assign(URL, { createObjectURL: vi.fn(() => "blob:x"), revokeObjectURL: vi.fn() }),
    );
    const names: string[] = [];
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      names.push(this.download);
    });
    mount(copyWith(COMPOSE));
    await flush();
    screen.getByRole("textbox", { name: "Rendered stack, read only" });
    fireEvent.click(screen.getByRole("button", { name: "Download" }));
    expect(names).toEqual(["_etc_x.rendered.yaml"]);
    click.mockRestore();
  });
});
