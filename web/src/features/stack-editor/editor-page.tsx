// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { CircleX } from "lucide-react";
import { type KeyboardEvent, useEffect, useReducer, useRef, useState } from "react";
import { Trans } from "react-i18next";

import { draftQuery, type StackDraftSaved, stackQuery, useSaveDraft } from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Kbd } from "@/components/ui/kbd";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { EditorFooter } from "@/features/service-editor/editor-footer";
import { FileEditor } from "@/features/stack-editor/file-editor";
import { SaveStatus } from "@/features/stack-editor/save-status";
import {
  COMPOSE_FILE,
  emptyWorkingCopy,
  isDirty,
  toSaveBody,
  type WorkingCopy,
  workingCopyReducer,
} from "@/features/stack-editor/working-copy";
import { usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { isMacPlatform } from "@/lib/env-nav";
import { toastSuccess } from "@/lib/toast";

export type EditorPane = "files" | "variables" | "rendered";

interface StackEditorPageProps {
  envId: string;
  stackId: string;
  // The pane the address names. Only Files exists so far; Variables and Rendered arrive with
  // their plans.
  pane?: EditorPane;
  // The file the address names, or none for compose.yaml.
  file?: string;
  // What to do to change the file in the address.
  onFileChange?: (path: string | undefined) => void;
}

// Where the saved draft stands: the version to send with the next save (the one read when the
// editor loaded the draft, or the one the last save answered with, never a version read since),
// and who saved it last and when.
interface Base {
  version: number;
  savedAt: string;
  savedBy: string;
}

// StackEditorPage is the Editor tab of a stack (STK-01, STK-05, D-08): the files of the stack's
// one shared draft in a CodeMirror surface. Edits stay in the browser until Save draft, with no
// autosave; the saved draft is shared and shows who saved it and when. A Viewer reads the files
// and cannot change them. Saving sends nothing to any engine: only a deploy does.
export function StackEditorPage({ envId, stackId, file }: StackEditorPageProps) {
  const t = useT();
  const mac = isMacPlatform();
  const canEdit = usePermission("stacks.update", envId);
  const stack = useQuery(stackQuery(envId, stackId));
  const draft = useQuery(draftQuery(envId, stackId));
  const save$ = useSaveDraft(envId, stackId);
  const [copy, dispatch] = useReducer(workingCopyReducer, emptyWorkingCopy);
  // The copy last loaded or saved: what "unsaved" is measured against.
  const [saved, setSaved] = useState<WorkingCopy>(emptyWorkingCopy);
  const [base, setBase] = useState<Base | null>(null);
  // A save in this visit changed the draft, so it is a draft now even before the list says so.
  const [savedChange, setSavedChange] = useState(false);
  const [saveError, setSaveError] = useState<"generic" | "lost" | "permission" | null>(null);
  const loaded = useRef(false);
  const copyRef = useRef(copy);
  copyRef.current = copy;

  const name = stack.data?.name ?? "";

  // The first read of the draft fills the working copy. The draft is read once; later changes to
  // the cached draft (a save answering) must not overwrite what is being typed.
  useEffect(() => {
    if (!draft.data || loaded.current) return;
    loaded.current = true;
    const next = workingCopyReducer(emptyWorkingCopy, { type: "load", draft: draft.data });
    dispatch({ type: "load", draft: draft.data });
    setSaved(next);
    setBase({
      version: draft.data.version,
      savedAt: draft.data.savedAt,
      savedBy: draft.data.savedBy,
    });
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

  const dirty = copy.loaded && isDirty(saved, copy);
  const open = copy.files.find((f) => f.path === copy.selected);
  const hasSaved = (stack.data?.hasDraft ?? false) || savedChange;

  function onSaved(sent: WorkingCopy, answer: StackDraftSaved) {
    setSaved(sent);
    setBase({ version: answer.version, savedAt: answer.savedAt, savedBy: answer.savedBy });
    if (answer.changed) setSavedChange(true);
    setSaveError(null);
    toastSuccess(t("stackEditor.save.toast", { name }));
  }

  function save() {
    if (!canEdit || !base || !copy.loaded || save$.isPending) return;
    const sent = copy;
    setSaveError(null);
    save$.mutate(toSaveBody(sent, base.version), {
      onSuccess: (answer) => onSaved(sent, answer),
      onError: (err) => {
        if (err.status === 0) setSaveError("lost");
        else if (err.status === 403) setSaveError("permission");
        else setSaveError("generic");
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
      <Tabs value="files">
        <TabsList
          variant="line"
          aria-label={t("stackEditor.panes.label")}
          className="h-9 max-md:h-11"
        >
          <TabsTrigger value="files" className="max-md:h-11">
            {t("stackEditor.panes.files")}
          </TabsTrigger>
        </TabsList>
      </Tabs>
      <div className="flex min-w-0 flex-col gap-2">
        {ready && open ? (
          open.hidden ? (
            <div className="rounded-lg border p-4">
              <p className="font-medium break-all">
                {t("stackEditor.secretFile.title", { file: open.path })}
              </p>
              <p className="text-sm text-muted-foreground">{t("stackEditor.secretFile.body")}</p>
            </div>
          ) : (
            <FileEditor
              key={open.path}
              file={open}
              readOnly={!canEdit}
              onChange={(content) => dispatch({ type: "edit", path: open.path, content })}
            />
          )
        ) : (
          <div role="status" aria-label={t("stackEditor.loading")}>
            <Skeleton className="h-100 w-full" />
          </div>
        )}
        <p className="text-sm text-muted-foreground">
          <Trans
            i18nKey="stackEditor.editor.helper"
            ns="stackEditor"
            values={keys}
            components={{ kbd: <Kbd /> }}
          />
        </p>
      </div>
      {saveError ? (
        <Alert variant="destructive">
          <CircleX aria-hidden="true" />
          <AlertDescription className="break-words">
            {t(`stackEditor.save.errors.${saveError}`)}
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
          <Button
            type="button"
            variant="outline"
            className="max-md:h-11"
            disabled={!ready}
            aria-disabled={pending ? true : undefined}
            onClick={pending ? undefined : save}
          >
            {pending ? (
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
        </EditorFooter>
      ) : (
        <p className="text-sm text-muted-foreground">{t("stackEditor.readOnly")}</p>
      )}
    </div>
  );
}
