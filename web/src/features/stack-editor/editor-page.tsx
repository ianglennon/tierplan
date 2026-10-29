// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useBlocker } from "@tanstack/react-router";
import { CircleX, TriangleAlert } from "lucide-react";
import { type KeyboardEvent, useEffect, useMemo, useReducer, useRef, useState } from "react";
import { Trans } from "react-i18next";

import {
  type DraftRequestError,
  draftQuery,
  revealFile,
  revealVariable,
  type StackDraft,
  type StackDraftSaved,
  type StackProblem,
  stackQuery,
  useDiscardDraft,
  useSaveDraft,
} from "@/api/stacks";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Kbd } from "@/components/ui/kbd";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { FileEditor, type FileEditorHandle } from "@/features/stack-editor/file-editor";
import { FileRail } from "@/features/stack-editor/file-rail";
import { ProblemsPanel } from "@/features/stack-editor/problems-panel";
import { RenderedView } from "@/features/stack-editor/rendered-view";
import { SaveStatus } from "@/features/stack-editor/save-status";
import { SecretFileCard } from "@/features/stack-editor/secret-file-card";
import { useStackCheck } from "@/features/stack-editor/use-stack-check";
import type { VarName } from "@/features/stack-editor/var-completion";
import { VariablesTable, type VariablesTableHandle } from "@/features/stack-editor/variables-table";
import {
  COMPOSE_FILE,
  emptyWorkingCopy,
  isDirty,
  toSaveBody,
  type WorkingCopy,
  withSecretsHidden,
  workingCopyReducer,
} from "@/features/stack-editor/working-copy";
import { usePermission } from "@/hooks/use-me";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { isMacPlatform } from "@/lib/env-nav";
import { toastSuccess } from "@/lib/toast";

export type EditorPane = "files" | "variables" | "rendered";

interface StackEditorPageProps {
  envId: string;
  stackId: string;
  // The pane the address names: Files (the default), Variables or Rendered.
  pane?: EditorPane;
  // What to do to change the pane in the address, and with it the file when the change opens one
  // (a "Used in" link): both in one move, so neither undoes the other.
  onPaneChange?: (pane: EditorPane, file?: string) => void;
  // The file the address names, or none for compose.yaml.
  file?: string;
  // What to do to change the file in the address.
  onFileChange?: (path: string | undefined) => void;
  // A line the address names (a link from the Review's list of errors): the cursor goes there
  // once, in the file the address names.
  line?: number;
  // What to do once the line has been used, so the address no longer carries it.
  onLineHandled?: () => void;
  // What to do to open the Review once the draft is saved: the route navigates to it.
  onReview?: () => void;
}

// Where the saved draft stands: the version to send with the next save (the one read when the
// editor loaded the draft, or the one the last save answered with, never a version read since),
// and who saved it last and when.
interface Base {
  version: number;
  savedAt: string;
  savedBy: string;
}

// A save the server refused because the draft moved: who moved it (empty when it was discarded)
// and the version it is at now, which "Save over it" sends.
interface Conflict {
  user: string;
  version: number;
}

// StackEditorPage is the Editor tab of a stack (STK-01, STK-05, D-08): the files of the stack's
// one shared draft in a CodeMirror surface. Edits stay in the browser until Save draft, with no
// autosave; the saved draft is shared and shows who saved it and when. A Viewer reads the files
// and cannot change them. Saving sends nothing to any engine: only a deploy does.
export function StackEditorPage({
  envId,
  stackId,
  pane,
  onPaneChange,
  file,
  onFileChange,
  line,
  onLineHandled,
  onReview,
}: StackEditorPageProps) {
  const t = useT();
  const activePane: EditorPane = pane === "variables" || pane === "rendered" ? pane : "files";
  const mac = isMacPlatform();
  const canEdit = usePermission("stacks.update", envId);
  const canReveal = usePermission("secrets.reveal", envId);
  const deployNeeds = useNeeds("stacks.deploy", envId);
  const stack = useQuery(stackQuery(envId, stackId));
  // A stack from a git repository: its Files tab shows the repository's files read-only and Bayplan's own
  // overlay files, and has no compose editor (D-09, D-11).
  const fromGit = stack.data?.source === "git";
  const draft = useQuery(draftQuery(envId, stackId));
  const save$ = useSaveDraft(envId, stackId);
  const discard$ = useDiscardDraft(envId, stackId);
  const [copy, dispatch] = useReducer(workingCopyReducer, emptyWorkingCopy);
  // The copy last loaded or saved: what "unsaved" is measured against.
  const [saved, setSaved] = useState<WorkingCopy>(emptyWorkingCopy);
  const [base, setBase] = useState<Base | null>(null);
  // A save in this visit changed the draft, so it is a draft now even before the list says so.
  const [savedChange, setSavedChange] = useState(false);
  // The sentence for the last refused save, shown above the footer until the next save.
  const [saveError, setSaveError] = useState<string | null>(null);
  const [conflict, setConflict] = useState<Conflict | null>(null);
  const [discardOpen, setDiscardOpen] = useState(false);
  const [discardError, setDiscardError] = useState<string | null>(null);
  // Saves in this visit: the Rendered pane renders again after each one.
  const [saveCount, setSaveCount] = useState(0);
  // "Show problems" in the Rendered pane opens the Problems panel of the Files pane it moves to.
  const [showProblems, setShowProblems] = useState(false);
  // Review and deploy has been pressed: it saves, then opens the Review. The button says Saving…
  // and then Checking… until the page is left (UI-SPEC "Editor" footer).
  const [reviewing, setReviewing] = useState(false);
  const loaded = useRef(false);
  const copyRef = useRef(copy);
  copyRef.current = copy;
  const check = useStackCheck(envId, stackId, copy);
  const unused = useMemo(() => new Set(check.unusedFiles), [check.unusedFiles]);
  // The files the compose file uses under secrets:, badged before a save has sealed them.
  const secretFiles = useMemo(() => new Set(check.secretFiles), [check.secretFiles]);
  // The names typing "${" offers: every variable of the table and every one the files refer to,
  // each with where its value comes from.
  const completionNames = useMemo(() => {
    const names = new Map<string, VarName>();
    const detail = (kind: "table" | "default" | "unset") => t(`stackVars.completion.${kind}`);
    for (const r of check.variables ?? []) {
      names.set(r.name, { name: r.name, detail: detail(r.hasDefault ? "default" : "unset") });
    }
    for (const v of copy.vars) {
      const has = v.value !== "" || v.hidden;
      const was = names.get(v.name);
      names.set(v.name, {
        name: v.name,
        detail: has ? detail("table") : (was?.detail ?? detail("unset")),
      });
    }
    return [...names.values()];
  }, [check.variables, copy.vars, t]);
  // The variables whose saved value the server holds hidden: the ones that can be revealed and, once
  // typed over, reset.
  const savedHidden = useMemo(
    () => new Set(saved.vars.filter((v) => v.hidden).map((v) => v.name)),
    [saved.vars],
  );
  const editor = useRef<FileEditorHandle>(null);
  const variablesPane = useRef<VariablesTableHandle>(null);
  // A click on a problem: the file to open and where to put the cursor once its editor is there.
  const [jump, setJump] = useState<{ file: string; line: number; col: number } | null>(null);

  const name = stack.data?.name ?? "";

  // applyLoaded makes a draft the server sent the working copy, the copy "unsaved" is measured
  // against, and the version the next save is made at. The open file stays open if it is there.
  function applyLoaded(data: StackDraft) {
    dispatch({ type: "load", draft: data });
    setSaved(workingCopyReducer(copyRef.current, { type: "load", draft: data }));
    setBase({ version: data.version, savedAt: data.savedAt, savedBy: data.savedBy });
    setSavedChange(false);
    setSaveError(null);
    setConflict(null);
  }

  // The first read of the draft fills the working copy. The draft is read once; later changes to
  // the cached draft (a save answering) must not overwrite what is being typed. Reading it again
  // is the person's choice (Reload draft, Discard draft).
  // biome-ignore lint/correctness/useExhaustiveDependencies: applyLoaded reads refs and setters only, and this runs once
  useEffect(() => {
    if (!draft.data || loaded.current) return;
    loaded.current = true;
    applyLoaded(draft.data);
  }, [draft.data]);

  // The file in the address opens when it is there; the address follows the person's choice.
  const urlFile = file ?? COMPOSE_FILE;
  const copyLoaded = copy.loaded;
  useEffect(() => {
    const current = copyRef.current;
    if (
      copyLoaded &&
      current.selected !== urlFile &&
      current.files.some((f) => f.path === urlFile)
    ) {
      dispatch({ type: "select", path: urlFile });
    }
  }, [urlFile, copyLoaded]);

  // The panel is opened for the one visit to the Files pane that "Show problems" asked for; after
  // that it stays as the person leaves it.
  useEffect(() => {
    if (activePane === "files") setShowProblems(false);
  }, [activePane]);

  const dirty = copy.loaded && isDirty(saved, copy);
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  // Leaving the page with unsaved edits asks first, through the router and, for the tab or the
  // window, through the browser. Moving between files changes the address but not the page, so
  // only a change of path asks (D-08).
  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) => dirtyRef.current && current.pathname !== next.pathname,
    enableBeforeUnload: () => dirtyRef.current,
    withResolver: true,
  });
  const open = copy.files.find((f) => f.path === copy.selected);
  // The problems of the open file, for the markers in its gutter; the panel lists them all.
  const openPath = open?.path;
  const markers = useMemo(
    () => check.problems.filter((p) => p.file === openPath),
    [check.problems, openPath],
  );
  // The jump waits for its file to be the open one, then moves the cursor and takes the focus. A
  // file with no editor (a hidden secret) has nowhere to put a cursor, so the jump ends there.
  useEffect(() => {
    if (!jump || copy.selected !== jump.file || activePane !== "files") return;
    if (editor.current) editor.current.cursorTo(jump.line, jump.col);
    setJump(null);
  }, [jump, copy.selected, activePane]);
  // The line the address names is used once, when the draft is in: the cursor goes to it in the
  // file the address names, and the address is cleared of it.
  const lineHandled = useRef(onLineHandled);
  lineHandled.current = onLineHandled;
  useEffect(() => {
    if (line === undefined || !copyLoaded) return;
    setJump({ file: urlFile, line, col: 1 });
    lineHandled.current?.();
  }, [line, copyLoaded, urlFile]);
  const hasSaved = (stack.data?.hasDraft ?? false) || savedChange;

  // Moving to a file, or off one that is gone, moves the address with it. compose.yaml is the
  // address with no file.
  const showInAddress = (path: string) => onFileChange?.(path === COMPOSE_FILE ? undefined : path);

  function selectFile(path: string) {
    dispatch({ type: "select", path });
    showInAddress(path);
  }

  // Opening a problem selects its file and puts the cursor on its line (UI-SPEC "Lint and inline
  // errors"). A problem in a file that is no longer there selects nothing.
  function openProblem(p: StackProblem) {
    if (!copyRef.current.files.some((f) => f.path === p.file)) return;
    if (copyRef.current.selected !== p.file) selectFile(p.file);
    setJump({ file: p.file, line: p.line, col: p.col });
  }

  // Opening a place a variable is used: the Files pane with that file open and the cursor on the
  // line. The jump waits for the editor of the pane to be there.
  function openAt(path: string, line: number) {
    if (!copyRef.current.files.some((f) => f.path === path)) return;
    dispatch({ type: "select", path });
    onPaneChange?.("files", path);
    setJump({ file: path, line, col: 1 });
  }

  function addFile(path: string, content?: string) {
    dispatch({ type: "addFile", path, content });
    showInAddress(path);
  }

  // An overlay file is secret from the start: it is stored sealed and shown hidden after the save.
  function addOverlay(path: string, content: string) {
    dispatch({ type: "addFile", path, content, origin: "overlay", secret: true });
    showInAddress(path);
  }

  function renameFile(from: string, to: string) {
    dispatch({ type: "renameFile", from, to });
    if (copy.selected === from) showInAddress(to);
  }

  function deleteFile(path: string) {
    dispatch({ type: "deleteFile", path });
    if (copy.selected === path) showInAddress(COMPOSE_FILE);
  }

  // The sentence for a save the server refused: a file the stack's rules refuse is named, as the
  // rail names a name it refuses; nothing the person typed is lost.
  function refusal(err: DraftRequestError): string {
    const where = err.fields[0]?.location ?? "";
    switch (err.type) {
      case "file_path_invalid":
        return t("stackEditor.rail.errors.path");
      case "file_name_taken":
        return t("stackEditor.rail.errors.taken", { name: where });
      case "file_too_large":
        return t("stackEditor.rail.errors.large");
      case "stack_too_large":
        return t("stackEditor.save.errors.tooLarge");
      case "compose_missing":
        return t("stackEditor.save.errors.composeMissing");
      case "variable_name_invalid":
        return t("stackEditor.save.errors.variableName");
      case "variable_name_taken":
        return t("stackEditor.save.errors.variableTaken", { name: err.fields[0]?.message ?? "" });
      case "secret_too_short":
        return t("stackEditor.save.errors.secretTooShort", { name: err.fields[0]?.message ?? "" });
      case "repo_file_read_only":
        return t("stackEditor.save.errors.repoFileReadOnly", {
          repo: copyRef.current.git?.repoName || t("stackEditor.git.theRepository"),
        });
    }
    if (err.status === 0) return t("stackEditor.save.errors.lost");
    if (err.status === 403) return t("stackEditor.save.errors.permission");
    return t("stackEditor.save.errors.generic");
  }

  function onSaved(sent: WorkingCopy, answer: StackDraftSaved) {
    // The server holds the secrets now: the browser lets go of the text it typed for them.
    const sealed = answer.secretFiles ?? [];
    setSaved(withSecretsHidden(sent, sealed));
    dispatch({ type: "saved", sent, secretFiles: sealed });
    setBase({ version: answer.version, savedAt: answer.savedAt, savedBy: answer.savedBy });
    if (answer.changed) setSavedChange(true);
    setSaveError(null);
    setSaveCount((n) => n + 1);
    check.runNow();
    toastSuccess(t("stackEditor.save.toast", { name }));
  }

  // Save sends the working copy at the version the editor holds. Save over it sends it at the
  // version the server named in the conflict, which is how the person chooses their copy over
  // another's.
  function save(version?: number, then?: () => void) {
    if (!canEdit || !base || !copy.loaded || save$.isPending) return;
    const sent = copy;
    setSaveError(null);
    setConflict(null);
    save$.mutate(toSaveBody(sent, version ?? base.version), {
      onSuccess: (answer) => {
        onSaved(sent, answer);
        then?.();
      },
      onError: (err) => {
        setReviewing(false);
        if (err.status === 409 && err.type === "draft_conflict") {
          const named = (key: string) => err.fields.find((f) => f.location === key)?.message ?? "";
          setConflict({ user: named("savedBy"), version: Number(named("version")) || 0 });
        } else {
          setSaveError(refusal(err));
        }
      },
    });
  }

  // openReview leaves for the Review. The draft is saved by now, so the router's guard against
  // unsaved edits must not ask: it reads a ref the next render would clear.
  function openReview() {
    dirtyRef.current = false;
    onReview?.();
  }

  // Review and deploy saves the working copy first, so the Review is of what is on screen, and
  // opens the Review only if the save went through. A save that conflicts shows the conflict
  // alert and the Review does not open (D-08). With nothing unsaved there is nothing to save.
  function reviewAndDeploy() {
    if (!canEdit || !base || !copy.loaded || save$.isPending || reviewing) return;
    setReviewing(true);
    if (!dirty) {
      openReview();
      return;
    }
    save(undefined, openReview);
  }

  // Reload draft reads what is saved now and drops the edits.
  async function reload() {
    const [read] = await Promise.all([draft.refetch(), stack.refetch()]);
    if (read.data) applyLoaded(read.data);
  }

  function discard() {
    if (discard$.isPending) return;
    setDiscardError(null);
    discard$.mutate(undefined, {
      onSuccess: async () => {
        const read = await draft.refetch();
        if (read.data) applyLoaded(read.data);
        setDiscardOpen(false);
        toastSuccess(t("stackEditor.discard.toast", { name }));
      },
      onError: (err) => {
        if (err.status === 0) setDiscardError(t("stackEditor.discard.errors.lost"));
        else if (err.status === 403) setDiscardError(t("stackEditor.discard.errors.permission"));
        else setDiscardError(t("stackEditor.discard.errors.generic"));
      },
    });
  }

  // Ctrl+S, or Cmd+S on a Mac, saves the draft from inside the editor; the browser's own Save
  // page dialog would otherwise open (UI-SPEC "Code editor keyboard").
  function onKeyDown(e: KeyboardEvent<HTMLDivElement>) {
    if ((e.ctrlKey || e.metaKey) && !e.altKey && e.key.toLowerCase() === "s") {
      e.preventDefault();
      save();
    }
  }

  if (draft.isError && !draft.data) {
    return (
      <Alert variant="destructive">
        <CircleX aria-hidden="true" />
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("stackEditor.error.loadFailed", { name: name || stackId })}</span>
          <span>{t("stackEditor.error.body")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={draft.isFetching}
            onClick={() => void draft.refetch()}
          >
            {t("stackEditor.error.retry", { name: name || stackId })}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }

  const ready = copy.loaded && open !== undefined;
  const pending = save$.isPending;
  const keys = {
    indent: mac ? "⌘]" : "Ctrl+]",
    outdent: mac ? "⌘[" : "Ctrl+[",
    save: mac ? "⌘S" : "Ctrl+S",
  };

  return (
    // The key handler is on the page so it hears the editor's keys as they bubble.
    // biome-ignore lint/a11y/noStaticElementInteractions: a shortcut handler on the container of the editor, which is the focusable element
    <div className="flex flex-col gap-4" onKeyDown={onKeyDown}>
      <Tabs
        value={activePane}
        onValueChange={(next) => {
          // Bulk-edited text that does not parse keeps the person on the Variables pane (D-04).
          if (activePane === "variables" && variablesPane.current?.leave() === false) return;
          onPaneChange?.(next as EditorPane);
        }}
      >
        <TabsList
          variant="line"
          aria-label={t("stackEditor.panes.label")}
          className="h-9 max-md:h-11"
        >
          <TabsTrigger value="files" className="max-md:h-11">
            {t("stackEditor.panes.files")}
          </TabsTrigger>
          <TabsTrigger value="variables" className="max-md:h-11">
            {t("stackEditor.panes.variables")}
          </TabsTrigger>
          <TabsTrigger value="rendered" className="max-md:h-11">
            {t("stackEditor.panes.rendered")}
          </TabsTrigger>
        </TabsList>
      </Tabs>
      {activePane === "rendered" ? (
        <RenderedView
          envId={envId}
          stackId={stackId}
          copy={copy}
          saveCount={saveCount}
          onShowProblems={() => {
            setShowProblems(true);
            onPaneChange?.("files");
          }}
        />
      ) : activePane === "variables" ? (
        <VariablesTable
          ref={variablesPane}
          vars={copy.vars}
          resolved={check.variables}
          loading={!copy.loaded}
          failed={check.failed}
          readOnly={!canEdit}
          canReveal={canReveal}
          reveal={(varName) => revealVariable(envId, stackId, varName)}
          savedHidden={savedHidden}
          onOpen={openAt}
          onReset={(varName) => dispatch({ type: "resetVar", name: varName })}
          onAdd={(varName) => dispatch({ type: "addVar", name: varName })}
          onSet={(varName, value) => dispatch({ type: "setVar", name: varName, value })}
          onSecret={(varName, secret) => dispatch({ type: "setSecret", name: varName, secret })}
          onRemove={(varName) => dispatch({ type: "removeVar", name: varName })}
          onBulk={(rows) => dispatch({ type: "setVars", vars: rows })}
          onImport={(imported, mode, secret) => {
            dispatch({ type: "importVars", vars: imported, mode, secret });
            toastSuccess(t("stackImport.toast", { count: imported.length }));
          }}
        />
      ) : (
        <div className="flex flex-col gap-4">
          {fromGit && ready ? (
            <p className="text-sm text-muted-foreground">
              {copy.git ? (
                <Trans
                  i18nKey="stackEditor.git.caption"
                  ns="stackEditor"
                  values={{ repo: copy.git.repoName, sha: copy.git.commit.slice(0, 7) }}
                  components={{ mono: <span className="font-mono" /> }}
                />
              ) : (
                t("stackEditor.git.captionNoSource")
              )}
            </p>
          ) : null}
          <div className="grid gap-4 md:grid-cols-[16rem_minmax(0,1fr)]">
            {ready ? (
              <FileRail
                files={copy.files}
                selected={copy.selected}
                readOnly={!canEdit}
                unused={unused}
                secret={secretFiles}
                git={fromGit}
                onSelect={selectFile}
                onAdd={addFile}
                onAddOverlay={addOverlay}
                onRename={renameFile}
                onDelete={deleteFile}
              />
            ) : (
              <div data-testid="rail-skeleton" className="flex flex-col gap-2" aria-hidden="true">
                <Skeleton className="h-6 w-20" />
                <Skeleton className="h-8 w-full" />
                <Skeleton className="h-8 w-full" />
              </div>
            )}
            <div className="flex min-w-0 flex-col gap-2">
              {ready && open ? (
                open.hidden ? (
                  <SecretFileCard
                    key={open.path}
                    path={open.path}
                    canReveal={canReveal}
                    readOnly={!canEdit || open.origin === "repo"}
                    reveal={() => revealFile(envId, stackId, open.path)}
                    onReplace={() => dispatch({ type: "replaceFile", path: open.path })}
                    onUpload={(content) => dispatch({ type: "edit", path: open.path, content })}
                  />
                ) : (
                  <FileEditor
                    key={open.path}
                    ref={editor}
                    file={open}
                    readOnly={!canEdit || open.origin === "repo"}
                    problems={markers}
                    variableNames={completionNames}
                    onChange={(content) => dispatch({ type: "edit", path: open.path, content })}
                  />
                )
              ) : (
                <div role="status" aria-label={t("stackEditor.loading")}>
                  <Skeleton className="h-100 w-full" />
                </div>
              )}
              {ready ? (
                <ProblemsPanel
                  problems={check.problems}
                  checking={check.checking}
                  failed={check.failed}
                  checked={check.checked}
                  initiallyOpen={showProblems}
                  onOpen={openProblem}
                />
              ) : null}
              <p className="text-sm text-muted-foreground">
                <Trans
                  i18nKey="stackEditor.editor.helper"
                  ns="stackEditor"
                  values={keys}
                  components={{ kbd: <Kbd /> }}
                />
              </p>
            </div>
          </div>
        </div>
      )}
      {saveError ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="break-words">{saveError}</AlertDescription>
        </Alert>
      ) : null}
      {conflict ? (
        <Alert variant="warning">
          <TriangleAlert aria-hidden="true" />
          <AlertDescription className="flex flex-col gap-3 text-status-warning-text">
            <p className="break-words">
              {conflict.user
                ? t("stackEditor.conflict.saved", { user: conflict.user })
                : t("stackEditor.conflict.discarded")}
            </p>
            <div className="flex flex-wrap gap-2">
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11"
                disabled={draft.isFetching}
                onClick={() => void reload()}
              >
                {t("stackEditor.conflict.reload")}
              </Button>
              <Button
                type="button"
                variant="outline"
                className="max-md:h-11"
                aria-disabled={pending ? true : undefined}
                onClick={pending ? undefined : () => save(conflict.version)}
              >
                {t("stackEditor.conflict.over")}
              </Button>
            </div>
          </AlertDescription>
        </Alert>
      ) : null}
      {canEdit ? (
        <EditorFooter>
          <div className="mr-auto min-w-0">
            <SaveStatus
              dirty={dirty}
              savedAt={hasSaved ? base?.savedAt : undefined}
              savedBy={base?.savedBy}
            />
          </div>
          {hasSaved || dirty ? (
            <Button
              type="button"
              variant="ghost"
              className="max-md:h-11"
              disabled={!ready}
              onClick={() => {
                setDiscardError(null);
                setDiscardOpen(true);
              }}
            >
              {t("stackEditor.discard.button")}
            </Button>
          ) : null}
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!ready}
            aria-disabled={pending ? true : undefined}
            onClick={pending ? undefined : () => save()}
          >
            {pending && !reviewing ? (
              <>
                <Spinner aria-hidden="true" />
                {t("stackEditor.save.pending")}
              </>
            ) : (
              <>
                {t("stackEditor.save.button")}
                <Kbd aria-hidden="true" className="max-md:hidden">
                  {keys.save}
                </Kbd>
              </>
            )}
          </Button>
          <NeedsTooltip reason={deployNeeds.reason}>
            <Button
              type="button"
              className="max-md:h-11"
              disabled={!ready}
              aria-disabled={pending || reviewing ? true : undefined}
              onClick={pending || reviewing ? undefined : reviewAndDeploy}
            >
              {pending && reviewing ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("stackReview.editor.saving")}
                </>
              ) : reviewing ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("stackReview.editor.checking")}
                </>
              ) : (
                t("stackReview.editor.button")
              )}
            </Button>
          </NeedsTooltip>
        </EditorFooter>
      ) : (
        <p className="text-sm text-muted-foreground">{t("stackEditor.readOnly")}</p>
      )}
      <AlertDialog
        open={discardOpen}
        onOpenChange={(next) => {
          // A discard on its way is not interrupted by closing the dialog.
          if (!discard$.isPending) setDiscardOpen(next);
        }}
      >
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("stackEditor.discard.title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("stackEditor.discard.body")}</AlertDialogDescription>
          </AlertDialogHeader>
          {discardError ? (
            <Alert variant="destructive">
              <CircleX aria-hidden="true" />
              <AlertDescription className="break-words">{discardError}</AlertDescription>
            </Alert>
          ) : null}
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus disabled={discard$.isPending}>
              {t("stackEditor.discard.cancel")}
            </AlertDialogCancel>
            <AlertDialogAction
              variant="destructive"
              aria-disabled={discard$.isPending ? true : undefined}
              onClick={(e) => {
                // The dialog stays open until the discard has happened, or says why it did not.
                e.preventDefault();
                discard();
              }}
            >
              {discard$.isPending ? (
                <>
                  <Spinner aria-hidden="true" />
                  {t("stackEditor.discard.pending")}
                </>
              ) : (
                t("stackEditor.discard.confirm")
              )}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <AlertDialog
        open={blocker.status === "blocked"}
        onOpenChange={(next) => {
          if (!next) blocker.reset?.();
        }}
      >
        <AlertDialogContent className="sm:max-w-120">
          <AlertDialogHeader>
            <AlertDialogTitle>{t("stackEditor.guard.title")}</AlertDialogTitle>
            <AlertDialogDescription>{t("stackEditor.guard.body", { name })}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel autoFocus>{t("stackEditor.guard.keep")}</AlertDialogCancel>
            <AlertDialogAction onClick={() => blocker.proceed?.()}>
              {t("stackEditor.guard.discard")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
