// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { StackProblem } from "@/api/stacks";
import {
  mergeProblems,
  stackCheckTiming,
  useStackCheck,
  yamlProblems,
} from "@/features/stack-editor/use-stack-check";
import {
  emptyWorkingCopy,
  type WorkingCopy,
  workingCopyReducer,
} from "@/features/stack-editor/working-copy";
import "@/i18n";

// Checking the working copy while the person types (plan 07-06, STK-01, D-10). The clock is fake and
// the server is a stubbed fetch, so every delay is the one in the contract and nothing waits.

const GOOD = "services:\n  web:\n    image: nginx\n";
const BAD = "services:\n\tweb:\n    image: nginx\n";

const problem = (over: Partial<StackProblem> = {}): StackProblem => ({
  severity: "error",
  code: "schema_invalid",
  message: "from the server",
  file: "compose.yaml",
  line: 2,
  col: 1,
  endLine: 2,
  endCol: 2,
  ...over,
});

function copyWith(compose: string): WorkingCopy {
  return workingCopyReducer(emptyWorkingCopy, {
    type: "load",
    draft: {
      isDraft: true,
      version: 1,
      savedBy: "ada",
      savedAt: "2026-10-19T10:00:00Z",
      baseSeq: 0,
      files: [{ path: "compose.yaml", content: compose, secret: false }],
      vars: [],
    },
  });
}

let sent: unknown[];
let answer: () => Response | Promise<Response>;

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout"] });
  sent = [];
  answer = () => Response.json({ problems: [], unusedFiles: [] });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (req: Request) => {
      if (new URL(req.url).pathname === "/api/env/e1/stacks/s1/check") {
        sent.push(await req.clone().json());
        return answer();
      }
      return Response.json({}, { status: 404 });
    }),
  );
});

afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("yamlProblems", () => {
  it("is empty for YAML that reads", () => {
    expect(yamlProblems(GOOD)).toEqual([]);
  });

  it("gives the line of a syntax error in the sentence the server uses", () => {
    const [first, ...rest] = yamlProblems(BAD);
    expect(rest).toEqual([]);
    expect(first).toMatchObject({
      severity: "error",
      code: "yaml_invalid",
      file: "compose.yaml",
      line: 2,
    });
    expect(first?.message).toMatch(/^Line 2: /);
    expect(first?.message).not.toContain("\n");
  });

  it("reads every document of the file", () => {
    expect(yamlProblems("a: 1\n---\nb: [\n").length).toBeGreaterThan(0);
  });
});

describe("mergeProblems", () => {
  it("lets the server's problem replace the browser's on the same file and line", () => {
    const merged = mergeProblems(
      [problem({ message: "server" })],
      [problem({ message: "browser" }), problem({ line: 9, message: "browser, line 9" })],
    );
    expect(merged.map((p) => p.message)).toEqual(["server", "browser, line 9"]);
  });

  it("does not let a problem of another file stand in for the same line", () => {
    const merged = mergeProblems([problem({ file: ".env" })], [problem()]);
    expect(merged).toHaveLength(2);
  });
});

describe("useStackCheck", () => {
  function run(initial: WorkingCopy) {
    return renderHook(({ copy }) => useStackCheck("e1", "s1", copy), {
      initialProps: { copy: initial },
    });
  }

  async function flush() {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
  }

  it("checks a copy that has just been read at once, and a copy not yet read never", async () => {
    const notLoaded = run(emptyWorkingCopy);
    await flush();
    expect(sent).toHaveLength(0);
    notLoaded.unmount();

    const loaded = run(copyWith(GOOD));
    await flush();
    expect(sent).toHaveLength(1);
    expect(loaded.result.current.checked).toBe(true);
  });

  it("runs the browser's check 400 ms after the last edit and the server's at 800 ms", async () => {
    const view = run(copyWith(GOOD));
    await flush();
    expect(sent).toHaveLength(1);

    answer = () => Response.json({ problems: [problem({ message: "server" })], unusedFiles: [] });
    view.rerender({ copy: copyWith(BAD) });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(stackCheckTiming.clientMs - 1);
    });
    expect(view.result.current.problems).toEqual([]);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(view.result.current.problems.map((p) => p.code)).toEqual(["yaml_invalid"]);
    expect(sent).toHaveLength(1);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(stackCheckTiming.serverMs - stackCheckTiming.clientMs - 1);
    });
    expect(sent).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
    expect(sent).toHaveLength(2);
    // The server's problem on line 2 replaces the browser's on line 2.
    expect(view.result.current.problems.map((p) => p.message)).toEqual(["server"]);
  });

  it("restarts both waits on each keystroke", async () => {
    const view = run(copyWith(GOOD));
    await flush();
    view.rerender({ copy: copyWith(`${GOOD}# a\n`) });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(stackCheckTiming.serverMs - 100);
    });
    view.rerender({ copy: copyWith(`${GOOD}# ab\n`) });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(stackCheckTiming.serverMs - 100);
    });
    expect(sent).toHaveLength(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(100);
    });
    expect(sent).toHaveLength(2);
  });

  it("sends the working copy as the save shape without a version", async () => {
    run(copyWith(GOOD));
    await flush();
    expect(sent[0]).toEqual({
      files: [{ path: "compose.yaml", content: GOOD, secret: false }],
      vars: [],
    });
  });

  it("keeps the last results when a check fails, says so, and never retries by itself", async () => {
    answer = () => Response.json({ problems: [problem()], unusedFiles: ["a.env"] });
    const view = run(copyWith(GOOD));
    await flush();
    expect(view.result.current.problems).toHaveLength(1);

    answer = () => Response.json({ type: "x", title: "x", status: 500 }, { status: 500 });
    view.rerender({ copy: copyWith(`${GOOD}# b\n`) });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(stackCheckTiming.serverMs);
    });
    expect(view.result.current.failed).toBe(true);
    expect(view.result.current.checking).toBe(false);
    expect(view.result.current.problems.map((p) => p.message)).toEqual(["from the server"]);
    expect(view.result.current.unusedFiles).toEqual(["a.env"]);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(sent).toHaveLength(2);
  });

  it("ignores an answer that arrives after a newer check was sent", async () => {
    const releases: ((r: Response) => void)[] = [];
    answer = () => new Promise<Response>((resolve) => releases.push(resolve));
    const view = run(copyWith(GOOD));
    await flush();
    expect(view.result.current.checking).toBe(true);

    view.rerender({ copy: copyWith(`${GOOD}# c\n`) });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(stackCheckTiming.serverMs);
    });
    expect(releases).toHaveLength(2);

    await act(async () => {
      releases[1]?.(Response.json({ problems: [problem({ message: "new" })], unusedFiles: [] }));
      await vi.advanceTimersByTimeAsync(0);
    });
    await act(async () => {
      releases[0]?.(Response.json({ problems: [problem({ message: "old" })], unusedFiles: [] }));
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(view.result.current.problems.map((p) => p.message)).toEqual(["new"]);
    expect(view.result.current.checking).toBe(false);
  });

  it("checks at once when asked to, as after a save", async () => {
    const view = run(copyWith(GOOD));
    await flush();
    act(() => view.result.current.runNow());
    await flush();
    expect(sent).toHaveLength(2);
  });
});
