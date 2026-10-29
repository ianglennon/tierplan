// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { StackProblem } from "@/api/stacks";
import {
  AddOverlayDialog,
  type AddOverlayDialogProps,
} from "@/features/git-source/add-overlay-dialog";
import { OverlayFilesCard } from "@/features/git-source/overlay-files-card";
import "@/i18n";

// The Overlay files card and the Add overlay file dialog of the From git page (plan 09-16, GIT-08,
// D-09): files the repository must not hold, such as a secret, supplied in Bayplan and kept
// encrypted. The contents are write-only and live in this state alone.

afterEach(cleanup);

const problem = (over: Partial<StackProblem> = {}): StackProblem => ({
  severity: "error",
  code: "overlay_clash",
  message: "",
  file: "compose.yaml",
  line: 1,
  col: 1,
  endLine: 1,
  endCol: 2,
  ...over,
});

describe("OverlayFilesCard", () => {
  const mount = (over: Partial<Parameters<typeof OverlayFilesCard>[0]> = {}) => {
    const props = {
      missing: [],
      overlays: [],
      problems: [],
      onAdd: vi.fn(),
      onRemove: vi.fn(),
      ...over,
    };
    const view = render(<OverlayFilesCard {...props} />);
    return { ...view, props };
  };

  it("is absent when nothing refers to a missing path and no overlay file was added", () => {
    const { container } = mount();
    expect(container.textContent).toBe("");
    expect(screen.queryByRole("heading", { name: "Overlay files" })).toBeNull();
  });

  it("lists each missing path as Missing, with Add contents", () => {
    const { props } = mount({ missing: ["secrets/db_pw.txt", "conf/app.toml"] });
    expect(screen.getByRole("heading", { name: "Overlay files" })).toBeTruthy();
    expect(
      screen.getByText(
        "Files the repository doesn't have, such as a secret or a config. Bayplan keeps them encrypted.",
      ),
    ).toBeTruthy();
    const row = screen.getByText("secrets/db_pw.txt").closest("li") as HTMLElement;
    expect(within(row).getByText("Missing")).toBeTruthy();
    expect(within(row).getByText("secrets/db_pw.txt").tagName).toBe("CODE");
    fireEvent.click(within(row).getByRole("button", { name: /Add contents/ }));
    expect(props.onAdd).toHaveBeenCalledWith("secrets/db_pw.txt");
  });

  it("lists an overlay file that was added, without its contents, and lets it go", () => {
    const { props } = mount({ overlays: ["secrets/db_pw.txt"] });
    const row = screen.getByText("secrets/db_pw.txt").closest("li") as HTMLElement;
    expect(within(row).queryByText("Missing")).toBeNull();
    expect(within(row).getByText("Added")).toBeTruthy();
    fireEvent.click(within(row).getByRole("button", { name: "Remove secrets/db_pw.txt" }));
    expect(props.onRemove).toHaveBeenCalledWith("secrets/db_pw.txt");
  });

  it("shows the card for an added overlay file even when nothing is missing any more", () => {
    mount({ overlays: ["a.txt"], missing: [] });
    expect(screen.getByRole("heading", { name: "Overlay files" })).toBeTruthy();
  });

  it("names the path of a clash as a blocking row", () => {
    mount({
      overlays: ["db_pw.txt"],
      problems: [
        problem({
          file: "db_pw.txt",
          message:
            "db_pw.txt exists in the repository and as an overlay file. Remove the overlay file or rename it.",
        }),
      ],
    });
    const row = screen.getByText("db_pw.txt", { selector: "code" }).closest("li") as HTMLElement;
    expect(row.textContent).toContain("Blocking: ");
    expect(row.textContent).toContain(
      "db_pw.txt exists in the repository and as an overlay file. Remove the overlay file or rename it.",
    );
  });

  it("attaches a clash to the overlay it is about, not to one whose path begins the same (WR-E02)", () => {
    mount({
      overlays: ["secrets/db", "secrets/db.key"],
      problems: [
        problem({
          file: "secrets/db.key",
          message: "secrets/db.key exists in the repository and as an overlay file.",
        }),
      ],
    });
    const plain = screen.getByText("secrets/db", { selector: "code" }).closest("li") as HTMLElement;
    const clashing = screen
      .getByText("secrets/db.key", { selector: "code" })
      .closest("li") as HTMLElement;
    expect(plain.textContent).not.toContain("Blocking: ");
    expect(clashing.textContent).toContain("Blocking: ");
  });

  it("opens Add overlay file with no path from its own button", () => {
    const { props } = mount({ missing: ["x"] });
    fireEvent.click(screen.getByRole("button", { name: "Add overlay file" }));
    expect(props.onAdd).toHaveBeenCalledWith("");
  });
});

describe("AddOverlayDialog", () => {
  const mount = (over: Partial<AddOverlayDialogProps> = {}) => {
    const props: AddOverlayDialogProps = {
      existingRepoPaths: ["compose.yaml", ".env"],
      overlays: [],
      onAdd: vi.fn(),
      onClose: vi.fn(),
      ...over,
    };
    const view = render(<AddOverlayDialog {...props} />);
    return { ...view, props };
  };
  const path = () => screen.getByLabelText("Path") as HTMLInputElement;
  const contents = () => screen.getByLabelText("Contents") as HTMLTextAreaElement;
  const submit = () => fireEvent.click(screen.getByRole("button", { name: "Add overlay file" }));

  it("says what it does and takes a path and write-only contents", () => {
    mount();
    expect(screen.getByRole("dialog", { name: "Add an overlay file" })).toBeTruthy();
    expect(
      screen.getByText(
        "Fills a path the repository doesn't have. The contents are encrypted and never shown again.",
      ),
    ).toBeTruthy();
    expect(path().placeholder).toBe("secrets/db_pw.txt");
    expect(
      screen.getByText("Relative to the stack folder. No leading slash and no .."),
    ).toBeTruthy();
    expect(contents().getAttribute("autocomplete")).toBe("off");
    expect(contents().rows).toBe(8);
    expect(contents().className).toContain("font-mono");
  });

  it("takes the title and the path it is given", () => {
    mount({ title: "Add contents", initialPath: "secrets/db_pw.txt" });
    expect(screen.getByRole("dialog", { name: "Add contents" })).toBeTruthy();
    expect(path().value).toBe("secrets/db_pw.txt");
  });

  it.each(["/x", "a/../b", "..", "./x", "a//b", "a/", "a\\b", "c:x", "", "   "])(
    "refuses the path %j as outside the stack folder",
    (bad) => {
      const { props } = mount();
      fireEvent.change(path(), { target: { value: bad } });
      fireEvent.change(contents(), { target: { value: "s3cret" } });
      submit();
      expect(screen.getByText("Use a path inside the stack folder.")).toBeTruthy();
      expect(path().getAttribute("aria-invalid")).toBe("true");
      expect(props.onAdd).not.toHaveBeenCalled();
      expect(props.onClose).not.toHaveBeenCalled();
    },
  );

  it("refuses a path the repository holds, naming it", () => {
    const { props } = mount();
    fireEvent.change(path(), { target: { value: "compose.yaml" } });
    submit();
    expect(
      screen.getByText(
        "compose.yaml exists in the repository. Overlay files can only fill paths it doesn't have.",
      ),
    ).toBeTruthy();
    expect(props.onAdd).not.toHaveBeenCalled();
  });

  it("refuses a path that is already an overlay file", () => {
    const { props } = mount({ overlays: ["secrets/db_pw.txt"] });
    fireEvent.change(path(), { target: { value: "secrets/db_pw.txt" } });
    submit();
    expect(screen.getByText("That path is already an overlay file.")).toBeTruthy();
    expect(props.onAdd).not.toHaveBeenCalled();
  });

  it("drops the error as the path is edited", () => {
    mount();
    fireEvent.change(path(), { target: { value: "/x" } });
    submit();
    expect(screen.getByText("Use a path inside the stack folder.")).toBeTruthy();
    fireEvent.change(path(), { target: { value: "x" } });
    expect(screen.queryByText("Use a path inside the stack folder.")).toBeNull();
  });

  it("hands over the path and the contents, then closes", () => {
    const { props } = mount();
    fireEvent.change(path(), { target: { value: " secrets/db_pw.txt " } });
    fireEvent.change(contents(), { target: { value: "hunter2\n" } });
    submit();
    expect(props.onAdd).toHaveBeenCalledWith({ path: "secrets/db_pw.txt", content: "hunter2\n" });
    expect(props.onClose).toHaveBeenCalledTimes(1);
  });

  it("reads a chosen file into the contents, in memory", async () => {
    mount();
    const input = screen.getByTestId("overlay-file") as HTMLInputElement;
    fireEvent.change(input, {
      target: { files: [new File(["from a file"], "db_pw.txt", { type: "text/plain" })] },
    });
    await waitFor(() => expect(contents().value).toBe("from a file"));
    expect(screen.getByRole("button", { name: "Choose file" })).toBeTruthy();
  });

  it("refuses a chosen file over 1 MiB and contents over 1 MiB", async () => {
    const { props } = mount();
    const input = screen.getByTestId("overlay-file") as HTMLInputElement;
    fireEvent.change(input, {
      target: { files: [new File([new Uint8Array(1_048_577)], "big.bin")] },
    });
    expect((await screen.findByRole("alert")).textContent).toBe(
      "That file is over 1 MiB. Overlay files hold at most 1 MiB.",
    );
    expect(contents().value).toBe("");

    fireEvent.change(path(), { target: { value: "big.txt" } });
    fireEvent.change(contents(), { target: { value: "x".repeat(1_048_577) } });
    submit();
    expect(screen.getAllByRole("alert").length).toBeGreaterThan(0);
    expect(props.onAdd).not.toHaveBeenCalled();
  });

  it("cancels without handing anything over", () => {
    const { props } = mount();
    fireEvent.change(path(), { target: { value: "x" } });
    fireEvent.change(contents(), { target: { value: "s3cret" } });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(props.onClose).toHaveBeenCalledTimes(1);
    expect(props.onAdd).not.toHaveBeenCalled();
  });

  it("holds the contents nowhere but in its own state", () => {
    const { unmount } = mount();
    fireEvent.change(path(), { target: { value: "x" } });
    fireEvent.change(contents(), { target: { value: "s3cret-contents" } });
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(window.location.href).not.toContain("s3cret");
    unmount();
    // A dialog opened again starts empty.
    mount();
    expect(contents().value).toBe("");
    expect(path().value).toBe("");
    expect(document.body.textContent).not.toContain("s3cret-contents");
  });
});
