// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { PENDING_MAX_MS, usePendingRows } from "@/features/containers/pending";

interface R {
  id: string;
  state: string;
}

const key = (r: R) => r.id;
const fingerprint = (r: R) => r.state;

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

function setup(rows: R[]) {
  return renderHook(({ rows }) => usePendingRows(rows, { getKey: key, fingerprint }), {
    initialProps: { rows },
  });
}

const mark = (r: ReturnType<typeof setup>, ids: string[], verb: "stop" | "kill" = "stop") =>
  act(() =>
    r.result.current.api.mark(
      ids.map((id) => ({ key: id, name: id, fingerprint: "running" })),
      verb,
    ),
  );

describe("usePendingRows", () => {
  it("keeps a row pending while it looks the same", () => {
    const r = setup([{ id: "a", state: "running" }]);
    mark(r, ["a"]);
    expect(r.result.current.pending.get("a")?.verb).toBe("stop");
    r.rerender({ rows: [{ id: "a", state: "running" }] });
    expect(r.result.current.pending.has("a")).toBe(true);
  });

  it("announces what was marked once", () => {
    const r = setup([
      { id: "a", state: "running" },
      { id: "b", state: "running" },
    ]);
    expect(r.result.current.announcement).toBeNull();
    mark(r, ["a", "b"], "kill");
    expect(r.result.current.announcement).toMatchObject({ verb: "kill", names: ["a", "b"] });
  });

  it("clears a row when its state changes, and only that row", () => {
    const r = setup([
      { id: "a", state: "running" },
      { id: "b", state: "running" },
    ]);
    mark(r, ["a", "b"]);
    r.rerender({
      rows: [
        { id: "a", state: "exited" },
        { id: "b", state: "running" },
      ],
    });
    expect([...r.result.current.pending.keys()]).toEqual(["b"]);
  });

  it("clears a row that is gone", () => {
    const r = setup([{ id: "a", state: "running" }]);
    mark(r, ["a"]);
    r.rerender({ rows: [] });
    expect(r.result.current.pending.size).toBe(0);
  });

  it("clears a row after 30 s whatever happens", () => {
    const r = setup([{ id: "a", state: "running" }]);
    mark(r, ["a"]);
    act(() => {
      vi.advanceTimersByTime(PENDING_MAX_MS - 1);
    });
    expect(r.result.current.pending.has("a")).toBe(true);
    act(() => {
      vi.advanceTimersByTime(2);
    });
    expect(r.result.current.pending.has("a")).toBe(false);
  });

  it("releases the rows whose action failed at once", () => {
    const r = setup([
      { id: "a", state: "running" },
      { id: "b", state: "running" },
    ]);
    mark(r, ["a", "b"]);
    act(() => r.result.current.api.release(["a"]));
    expect([...r.result.current.pending.keys()]).toEqual(["b"]);
  });
});
