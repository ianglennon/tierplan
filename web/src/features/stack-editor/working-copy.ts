// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackDraft, StackDraftSaveInput } from "@/api/stacks";
import { HIDDEN } from "@/features/spec-editor/diff";

// The compose file's path: the one file a stack cannot do without. It is always first in the
// rail, and it can be neither renamed nor deleted (D-05).
export const COMPOSE_FILE = "compose.yaml";

// One file of the working copy. A secret file the server holds is sent without its content
// (hidden): the editor never has the text, and a file the person did not touch is saved as it is.
export interface WorkingFile {
  path: string;
  content: string;
  secret: boolean;
  hidden: boolean;
}

// One variable of the working copy, with the same rule for a hidden secret value.
export interface WorkingVar {
  name: string;
  value: string;
  secret: boolean;
  hidden: boolean;
}

// WorkingCopy is what the editor holds between the saved draft and the next save: every file and
// variable of the draft as the person has it now, and the file open in the editor. It lives in
// memory only; no text of it is ever written to storage (UI-SPEC "Storage").
export interface WorkingCopy {
  // False until the draft has been read. The editor shows skeletons until then.
  loaded: boolean;
  files: WorkingFile[];
  vars: WorkingVar[];
  selected: string;
}

export const emptyWorkingCopy: WorkingCopy = {
  loaded: false,
  files: [],
  vars: [],
  selected: COMPOSE_FILE,
};

export type WorkingCopyAction =
  // Replace everything with what the server holds. The open file stays open if it is still there.
  | { type: "load"; draft: StackDraft }
  // Replace the text of one file. Editing a hidden secret file replaces it.
  | { type: "edit"; path: string; content: string }
  | { type: "addFile"; path: string; content?: string }
  | { type: "renameFile"; from: string; to: string }
  | { type: "deleteFile"; path: string }
  | { type: "select"; path: string }
  // The variables table. A variable the compose file uses is created by the first value typed for
  // it; addVar is for a name the files do not refer to yet.
  | { type: "addVar"; name: string }
  // Typing a value. It replaces a hidden secret value, so the row stops being hidden.
  | { type: "setVar"; name: string; value: string }
  | { type: "removeVar"; name: string }
  // The Secret switch. Turning it off on a hidden value clears the value: the text the server
  // holds is never in the browser, so it cannot become plain text without being typed again.
  | { type: "setSecret"; name: string; secret: boolean }
  // Puts a hidden secret that was typed over back to the value the server holds.
  | { type: "resetVar"; name: string };

// compose.yaml first, then the rest in path order, so the rail and the save body are stable.
function ordered(files: WorkingFile[]): WorkingFile[] {
  return [...files].sort((a, b) => {
    if (a.path === COMPOSE_FILE) return -1;
    if (b.path === COMPOSE_FILE) return 1;
    return a.path < b.path ? -1 : a.path > b.path ? 1 : 0;
  });
}

// The variables by name, so the table and the save body are stable.
function orderedVars(vars: WorkingVar[]): WorkingVar[] {
  return [...vars].sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
}

// upsertVar changes the variable called name, or adds it when the table has none.
function upsertVar(
  vars: WorkingVar[],
  name: string,
  change: (v: WorkingVar) => WorkingVar,
): WorkingVar[] {
  const found = vars.some((v) => v.name === name);
  if (found) return vars.map((v) => (v.name === name ? change(v) : v));
  return orderedVars([...vars, change({ name, value: "", secret: false, hidden: false })]);
}

function fromDraft(draft: StackDraft, selected: string): WorkingCopy {
  const files = ordered(
    (draft.files ?? []).map((f) => ({
      path: f.path,
      content: f.secret && f.content === HIDDEN ? "" : f.content,
      secret: f.secret,
      hidden: f.secret && f.content === HIDDEN,
    })),
  );
  const vars = (draft.vars ?? []).map((v) => ({
    name: v.name,
    value: v.secret && v.value === HIDDEN ? "" : v.value,
    secret: v.secret,
    hidden: v.secret && v.value === HIDDEN,
  }));
  return {
    loaded: true,
    files,
    vars,
    selected: files.some((f) => f.path === selected) ? selected : COMPOSE_FILE,
  };
}

export function workingCopyReducer(state: WorkingCopy, action: WorkingCopyAction): WorkingCopy {
  switch (action.type) {
    case "load":
      return fromDraft(action.draft, state.selected);
    case "edit":
      return {
        ...state,
        files: state.files.map((f) =>
          f.path === action.path ? { ...f, content: action.content, hidden: false } : f,
        ),
      };
    case "addFile":
      if (state.files.some((f) => f.path === action.path)) return state;
      return {
        ...state,
        files: ordered([
          ...state.files,
          { path: action.path, content: action.content ?? "", secret: false, hidden: false },
        ]),
        selected: action.path,
      };
    case "renameFile": {
      if (action.from === COMPOSE_FILE || action.from === action.to) return state;
      if (state.files.some((f) => f.path === action.to)) return state;
      return {
        ...state,
        files: ordered(
          state.files.map((f) => (f.path === action.from ? { ...f, path: action.to } : f)),
        ),
        selected: state.selected === action.from ? action.to : state.selected,
      };
    }
    case "deleteFile":
      if (action.path === COMPOSE_FILE) return state;
      return {
        ...state,
        files: state.files.filter((f) => f.path !== action.path),
        selected: state.selected === action.path ? COMPOSE_FILE : state.selected,
      };
    case "select":
      return state.files.some((f) => f.path === action.path)
        ? { ...state, selected: action.path }
        : state;
    case "addVar":
      if (state.vars.some((v) => v.name === action.name)) return state;
      return { ...state, vars: upsertVar(state.vars, action.name, (v) => v) };
    case "setVar":
      return {
        ...state,
        vars: upsertVar(state.vars, action.name, (v) => ({
          ...v,
          value: action.value,
          hidden: false,
        })),
      };
    case "removeVar":
      return { ...state, vars: state.vars.filter((v) => v.name !== action.name) };
    case "setSecret":
      return {
        ...state,
        vars: upsertVar(state.vars, action.name, (v) =>
          !action.secret && v.hidden
            ? { ...v, secret: false, value: "", hidden: false }
            : { ...v, secret: action.secret },
        ),
      };
    case "resetVar":
      return {
        ...state,
        vars: state.vars.map((v) =>
          v.name === action.name && v.secret ? { ...v, value: "", hidden: true } : v,
        ),
      };
  }
}

// isDirty says whether the working copy differs from the copy last saved or loaded. The open file
// is not content: moving between files is not an edit.
export function isDirty(saved: WorkingCopy, current: WorkingCopy): boolean {
  if (saved.files.length !== current.files.length || saved.vars.length !== current.vars.length) {
    return true;
  }
  const files = new Map(saved.files.map((f) => [f.path, f]));
  for (const f of current.files) {
    const was = files.get(f.path);
    if (!was || was.content !== f.content || was.secret !== f.secret || was.hidden !== f.hidden) {
      return true;
    }
  }
  const vars = new Map(saved.vars.map((v) => [v.name, v]));
  for (const v of current.vars) {
    const was = vars.get(v.name);
    if (!was || was.value !== v.value || was.secret !== v.secret || was.hidden !== v.hidden) {
      return true;
    }
  }
  return false;
}

// toSaveBody is the body of a save. A hidden secret file or value the person did not touch goes
// as the server's keep marker, <hidden>, which the server answers by keeping what it holds; the
// text of a hidden value is not in the working copy, so it cannot be sent.
export function toSaveBody(copy: WorkingCopy, version: number): StackDraftSaveInput {
  return {
    version,
    files: copy.files.map((f) => ({
      path: f.path,
      content: f.hidden ? HIDDEN : f.content,
      secret: f.secret,
    })),
    vars: copy.vars.map((v) => ({
      name: v.name,
      value: v.hidden ? HIDDEN : v.value,
      secret: v.secret,
    })),
  };
}

export type FilePathProblem = "invalid" | "taken";

// The characters no path may hold: a backslash (a Windows separator), a colon (a drive, or the
// separator of a bind mount) and the control, format, line-separator and unpaired-surrogate
// characters that make one name look like another or break a line.
const BAD_PATH_CHAR = /[\\:\p{Cc}\p{Cf}\p{Zl}\p{Zp}\p{Cs}]/u;

// The most characters in a path and in one name of it, counted as code points like the server.
export const MAX_PATH_CHARS = 255;
export const MAX_SEGMENT_CHARS = 100;

// The most a stack file holds, in bytes (stacks.MaxFileBytes).
export const MAX_FILE_BYTES = 1 << 20;

function validFilePath(path: string): boolean {
  if (path === "" || path.startsWith("/") || BAD_PATH_CHAR.test(path)) return false;
  if ([...path].length > MAX_PATH_CHARS) return false;
  return path
    .split("/")
    .every(
      (name) =>
        name !== "" && name !== "." && name !== ".." && [...name].length <= MAX_SEGMENT_CHARS,
    );
}

// filePathProblem says why a name cannot be added as a file of the stack, or null. It is the
// browser's copy of the server's rule (stacks.ValidPath), so a bad name is caught before it
// appears; the server holds every save to the rule whatever the browser did. A path that breaks
// the rule is invalid even if it is also taken.
export function filePathProblem(path: string, existing: readonly string[]): FilePathProblem | null {
  if (!validFilePath(path)) return "invalid";
  return existing.includes(path) ? "taken" : null;
}
