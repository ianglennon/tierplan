// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { FilePlus, MoreHorizontal } from "lucide-react";
import {
  type ChangeEvent,
  type FormEvent,
  type KeyboardEvent,
  useId,
  useRef,
  useState,
} from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Popover, PopoverContent, PopoverTitle, PopoverTrigger } from "@/components/ui/popover";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { AddOverlayDialog } from "@/features/git-source/add-overlay-dialog";
import {
  COMPOSE_FILE,
  type FilePathProblem,
  filePathProblem,
  MAX_FILE_BYTES,
  type WorkingFile,
} from "@/features/stack-editor/working-copy";
import { useIsMobile } from "@/hooks/use-mobile";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

export interface FileRailProps {
  files: WorkingFile[];
  // The open file's path.
  selected: string;
  // A Viewer reads the files and changes none.
  readOnly: boolean;
  // Paths the compose file does not refer to, which get the muted "Not used" label. The server's
  // check names them (plan 07-06); none until it has answered.
  unused?: ReadonlySet<string>;
  // Paths the compose file uses under secrets:, which are secret files even before a save has
  // sealed them (plan 07-13). They get the "Secret" badge a flagged file has.
  secret?: ReadonlySet<string>;
  // True for the files of a stack from a git repository: each carries a Repo or Overlay badge, only overlay files
  // can be renamed or deleted, and the Add file popover is Add overlay file (D-09, D-11).
  git?: boolean;
  onSelect: (path: string) => void;
  onAdd: (path: string, content?: string) => void;
  // A new overlay file of a git stack, with the contents typed for it.
  onAddOverlay?: (path: string, content: string) => void;
  onRename: (from: string, to: string) => void;
  onDelete: (path: string) => void;
}

// FileRail is the list of a stack's files beside the editor (UI-SPEC "Editor"): compose.yaml
// pinned first with no menu, then the rest, each a button that opens it. It is a navigation of
// buttons, not a listbox: the arrow keys, Home and End move between the files, Enter opens one,
// and the menu key or Shift+F10 opens the row menu (Rename…, Delete file). Under 768px the list
// is a labelled select above the editor. Every change is to the working copy; none is saved here.
export function FileRail({
  files,
  selected,
  readOnly,
  unused,
  secret,
  git = false,
  onSelect,
  onAdd,
  onAddOverlay,
  onRename,
  onDelete,
}: FileRailProps) {
  const t = useT();
  const mobile = useIsMobile();
  const selectId = useId();
  const [menuFor, setMenuFor] = useState<string | null>(null);
  const [renaming, setRenaming] = useState<string | null>(null);
  const rows = useRef(new Map<string, HTMLButtonElement>());
  const paths = files.map((f) => f.path);

  // compose.yaml has no menu, and a Viewer has none at all. In a git stack only an overlay file has one: the
  // repository's files are not Bayplan's to rename or delete.
  const canAct = (file: WorkingFile) =>
    !readOnly && (git ? file.origin === "overlay" : file.path !== COMPOSE_FILE);
  const hasOverlay = files.some((f) => f.origin === "overlay");
  const originLabel = (file: WorkingFile) =>
    file.origin ? t(`stackEditor.git.origin.${file.origin}`) : "";

  function focusRow(index: number) {
    const target = files[Math.min(Math.max(index, 0), files.length - 1)];
    if (target) rows.current.get(target.path)?.focus();
  }

  function onRowKeyDown(e: KeyboardEvent<HTMLButtonElement>, index: number, file: WorkingFile) {
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        focusRow(index + 1);
        break;
      case "ArrowUp":
        e.preventDefault();
        focusRow(index - 1);
        break;
      case "Home":
        e.preventDefault();
        focusRow(0);
        break;
      case "End":
        e.preventDefault();
        focusRow(files.length - 1);
        break;
      case "ContextMenu":
      case "F10":
        // The menu key opens the menu; F10 does only with Shift held (Shift+F10).
        if ((e.key === "ContextMenu" || e.shiftKey) && canAct(file)) {
          e.preventDefault();
          setMenuFor(file.path);
        }
        break;
    }
  }

  const open = files.find((f) => f.path === selected);
  const menu = (file: WorkingFile) => (
    <RowMenu
      file={file}
      open={menuFor === file.path}
      onOpenChange={(next) => setMenuFor(next ? file.path : null)}
      onRename={() => setRenaming(file.path)}
      onDelete={() => onDelete(file.path)}
    />
  );

  return (
    <nav aria-label={t("stackEditor.rail.label")} className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <h3 className="text-sm font-medium">{t("stackEditor.rail.heading")}</h3>
        {readOnly ? null : git ? (
          <AddOverlay files={files} onAdd={(path, content) => onAddOverlay?.(path, content)} />
        ) : (
          <AddFile paths={paths} onAdd={onAdd} />
        )}
      </div>
      {mobile ? (
        <div data-testid="file-select" className="flex items-end gap-2">
          <div className="flex min-w-0 flex-1 flex-col gap-1">
            <Label htmlFor={selectId}>{t("stackEditor.rail.select")}</Label>
            <select
              id={selectId}
              value={selected}
              onChange={(e) => onSelect(e.target.value)}
              className="h-11 w-full min-w-0 rounded-lg border border-input bg-transparent px-2.5 font-mono text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50"
            >
              {files.map((f) => (
                <option key={f.path} value={f.path}>
                  {[
                    f.path,
                    ...(f.origin ? [`(${originLabel(f)})`] : []),
                    ...(unused?.has(f.path) ? [`(${t("stackEditor.rail.notUsed")})`] : []),
                  ].join(" ")}
                </option>
              ))}
            </select>
          </div>
          {open && canAct(open) ? menu(open) : null}
        </div>
      ) : (
        <ul className="flex flex-col gap-0.5">
          {files.map((file, index) => (
            <li key={file.path} className="flex items-center gap-1">
              <Tooltip>
                <TooltipTrigger asChild>
                  <button
                    type="button"
                    data-file={file.path}
                    ref={(node) => {
                      if (node) rows.current.set(file.path, node);
                      else rows.current.delete(file.path);
                    }}
                    aria-current={file.path === selected ? "true" : undefined}
                    onClick={() => onSelect(file.path)}
                    onKeyDown={(e) => onRowKeyDown(e, index, file)}
                    className={cn(
                      "flex min-w-0 flex-1 items-center gap-2 rounded-md px-2 py-1.5 text-left font-mono text-sm outline-none hover:bg-muted focus-visible:ring-3 focus-visible:ring-ring/50",
                      file.path === selected && "bg-muted font-medium",
                    )}
                  >
                    <span className="truncate">{file.path}</span>
                    {file.origin ? (
                      <Badge variant="outline" className="shrink-0 font-sans">
                        {originLabel(file)}
                      </Badge>
                    ) : null}
                    {(file.secret || secret?.has(file.path)) && file.origin !== "overlay" ? (
                      <Badge variant="outline" className="shrink-0 font-sans">
                        {t("stackEditor.rail.secret")}
                      </Badge>
                    ) : null}
                    {unused?.has(file.path) ? (
                      <span className="shrink-0 font-sans text-xs text-muted-foreground">
                        {t("stackEditor.rail.notUsed")}
                      </span>
                    ) : null}
                  </button>
                </TooltipTrigger>
                <TooltipContent className="break-all">{file.path}</TooltipContent>
              </Tooltip>
              {canAct(file) ? menu(file) : null}
            </li>
          ))}
        </ul>
      )}
      {git ? (
        hasOverlay ? null : (
          <p className="text-sm text-muted-foreground">{t("stackEditor.git.noOverlays")}</p>
        )
      ) : files.length === 1 && !readOnly ? (
        <p className="text-sm text-muted-foreground">{t("stackEditor.rail.empty")}</p>
      ) : null}
      {renaming ? (
        <RenameDialog
          from={renaming}
          paths={paths}
          onClose={() => setRenaming(null)}
          onRename={onRename}
        />
      ) : null}
    </nav>
  );
}

// RowMenu is the "Actions for {file}" menu of one file. A secret file whose content is hidden
// cannot be renamed: the server keeps a hidden secret by its path, so a new path would have no
// content to keep.
function RowMenu({
  file,
  open,
  onOpenChange,
  onRename,
  onDelete,
}: {
  file: WorkingFile;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onRename: () => void;
  onDelete: () => void;
}) {
  const t = useT();
  return (
    <DropdownMenu open={open} onOpenChange={onOpenChange}>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="shrink-0 max-md:size-11"
          aria-label={t("stackEditor.rail.actions", { file: file.path })}
        >
          <MoreHorizontal aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {file.hidden ? null : (
          <DropdownMenuItem className="max-md:min-h-11" onSelect={onRename}>
            {t("stackEditor.rail.rename")}
          </DropdownMenuItem>
        )}
        <DropdownMenuItem variant="destructive" className="max-md:min-h-11" onSelect={onDelete}>
          {t("stackEditor.rail.delete")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

// AddOverlay is the Add overlay file button of a git stack and the shared dialog it opens (D-09): a path the
// repository does not hold, and the contents, which are write-only. The dialog is rendered while it is open, so
// closing it lets go of what was typed.
function AddOverlay({
  files,
  onAdd,
}: {
  files: readonly WorkingFile[];
  onAdd: (path: string, content: string) => void;
}) {
  const t = useT();
  const [open, setOpen] = useState(false);
  return (
    <>
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="max-md:h-11"
        onClick={() => setOpen(true)}
      >
        <FilePlus aria-hidden="true" />
        {t("stackEditor.git.addOverlay")}
      </Button>
      {open ? (
        <AddOverlayDialog
          existingRepoPaths={files.filter((f) => f.origin !== "overlay").map((f) => f.path)}
          overlays={files.filter((f) => f.origin === "overlay").map((f) => f.path)}
          title={t("stackEditor.git.addOverlay")}
          onAdd={(overlay) => onAdd(overlay.path, overlay.content)}
          onClose={() => setOpen(false)}
        />
      ) : null}
    </>
  );
}

// pathMessage is the sentence for a name the rail refuses.
function pathMessage(t: ReturnType<typeof useT>, problem: FilePathProblem, name: string): string {
  return problem === "taken"
    ? t("stackEditor.rail.errors.taken", { name })
    : t("stackEditor.rail.errors.path");
}

const ERROR_CLASS = "text-sm break-all text-destructive";

// AddFile is the "Add file" button and the popover it opens: a name field checked by the same
// rule as the server's, and an upload that adds each chosen file under its own name. A name that
// is refused stays in the field with the reason beside it; nothing appears in the rail until the
// name is good.
function AddFile({
  paths,
  onAdd,
}: {
  paths: string[];
  onAdd: (path: string, content?: string) => void;
}) {
  const t = useT();
  const ids = useId();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [errors, setErrors] = useState<string[]>([]);
  const chooser = useRef<HTMLInputElement>(null);

  // Every way out of the popover (Cancel, Escape, a click outside, a file added) leaves it empty.
  function setOpenAndReset(next: boolean) {
    setOpen(next);
    if (!next) {
      setName("");
      setErrors([]);
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    const path = name.trim();
    const problem = filePathProblem(path, paths);
    if (problem) {
      setErrors([pathMessage(t, problem, path)]);
      return;
    }
    onAdd(path);
    setOpenAndReset(false);
  }

  // Each uploaded file is added under its own name, one after another, so two with the same name
  // are caught as the second of them. A file that cannot be added is named in the popover and
  // the rest still are.
  async function upload(e: ChangeEvent<HTMLInputElement>) {
    const chosen = Array.from(e.target.files ?? []);
    e.target.value = "";
    const known = [...paths];
    const refused: string[] = [];
    for (const file of chosen) {
      const problem = filePathProblem(file.name, known);
      if (problem) {
        refused.push(pathMessage(t, problem, file.name));
      } else if (file.size > MAX_FILE_BYTES) {
        refused.push(t("stackEditor.rail.errors.large"));
      } else {
        onAdd(file.name, await file.text());
        known.push(file.name);
      }
    }
    if (refused.length === 0) {
      setOpenAndReset(false);
    } else {
      setErrors(refused);
    }
  }

  return (
    <Popover open={open} onOpenChange={setOpenAndReset}>
      <PopoverTrigger asChild>
        <Button type="button" variant="outline" size="sm" className="max-md:h-11">
          <FilePlus aria-hidden="true" />
          {t("stackEditor.rail.add.button")}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-80" aria-labelledby={`${ids}-title`}>
        <PopoverTitle id={`${ids}-title`}>{t("stackEditor.rail.add.title")}</PopoverTitle>
        <form noValidate onSubmit={submit} className="flex flex-col gap-2">
          <Label htmlFor={`${ids}-name`}>{t("stackEditor.rail.add.label")}</Label>
          <Input
            id={`${ids}-name`}
            value={name}
            placeholder={t("stackEditor.rail.add.placeholder")}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            spellCheck={false}
            aria-invalid={errors.length > 0}
            aria-describedby={`${ids}-helper`}
            className="max-md:h-11"
            onChange={(e) => {
              setName(e.target.value);
              setErrors([]);
            }}
          />
          <p id={`${ids}-helper`} className="text-xs break-words text-muted-foreground">
            {t("stackEditor.rail.add.helper")}
          </p>
          {errors.map((message) => (
            <p key={message} role="alert" className={ERROR_CLASS}>
              {message}
            </p>
          ))}
          <input
            ref={chooser}
            type="file"
            multiple
            className="sr-only"
            tabIndex={-1}
            aria-hidden="true"
            onChange={(e) => void upload(e)}
          />
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="max-md:h-11"
              onClick={() => chooser.current?.click()}
            >
              {t("stackEditor.rail.add.upload")}
            </Button>
            <div className="flex gap-2">
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="max-md:h-11"
                onClick={() => setOpenAndReset(false)}
              >
                {t("stackEditor.rail.add.cancel")}
              </Button>
              <Button type="submit" size="sm" className="max-md:h-11">
                {t("stackEditor.rail.add.submit")}
              </Button>
            </div>
          </div>
        </form>
      </PopoverContent>
    </Popover>
  );
}

// RenameDialog asks for a new path for one file. Renaming a file does not rewrite what the
// compose file says about it (UI-SPEC "Files pane"), and nothing is saved until Save draft.
function RenameDialog({
  from,
  paths,
  onClose,
  onRename,
}: {
  from: string;
  paths: string[];
  onClose: () => void;
  onRename: (from: string, to: string) => void;
}) {
  const t = useT();
  const ids = useId();
  const [name, setName] = useState(from);
  const [error, setError] = useState<string | null>(null);

  function submit(e: FormEvent) {
    e.preventDefault();
    const path = name.trim();
    if (path === from) {
      onClose();
      return;
    }
    const problem = filePathProblem(
      path,
      paths.filter((p) => p !== from),
    );
    if (problem) {
      setError(pathMessage(t, problem, path));
      return;
    }
    onRename(from, path);
    onClose();
  }

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="sm:max-w-100">
        <DialogHeader>
          <DialogTitle className="break-all">
            {t("stackEditor.rail.renameDialog.title", { file: from })}
          </DialogTitle>
          <DialogDescription>{t("stackEditor.rail.add.helper")}</DialogDescription>
        </DialogHeader>
        <form noValidate onSubmit={submit} className="flex flex-col gap-3">
          <div className="flex flex-col gap-2">
            <Label htmlFor={`${ids}-name`}>{t("stackEditor.rail.add.label")}</Label>
            <Input
              id={`${ids}-name`}
              value={name}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              spellCheck={false}
              aria-invalid={error !== null}
              className="max-md:h-11"
              onChange={(e) => {
                setName(e.target.value);
                setError(null);
              }}
            />
            {error ? (
              <p role="alert" className={ERROR_CLASS}>
                {error}
              </p>
            ) : null}
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" className="max-md:h-11" onClick={onClose}>
              {t("stackEditor.rail.add.cancel")}
            </Button>
            <Button type="submit" className="max-md:h-11">
              {t("stackEditor.rail.renameDialog.submit")}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
