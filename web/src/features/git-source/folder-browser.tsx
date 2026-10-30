// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronRight, CircleX, FileText, Folder } from "lucide-react";
import { type KeyboardEvent, useEffect, useId, useRef, useState } from "react";

import { type GitRefKind, GitRequestError, type GitTreeEntry, gitTreeQuery } from "@/api/git";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { useBrowseReason } from "@/features/git-source/browse-reason";
import { TruncatedText } from "@/features/resources/resource-table";
import { useT } from "@/i18n/use-t";
import { formatCount } from "@/lib/format";
import { cn } from "@/lib/utils";

// The filter field appears from this many entries.
const FILTER_FROM = 13;
// How many rows stand in for a folder that is loading.
const SKELETON_ROWS = 6;

interface FolderBrowserProps {
  envId: string;
  repoId: string;
  repoName: string;
  kind: GitRefKind;
  gitRef: string;
  // The folder chosen as the stack's root; null while none is, "" for the repository root.
  folder: string | null;
  onUse: (path: string) => void;
}

const join = (path: string, name: string) => (path ? `${path}/${name}` : name);
const parentOf = (path: string) => path.slice(0, Math.max(path.lastIndexOf("/"), 0));
const isYaml = (name: string) => /\.ya?ml$/i.test(name);

// What the keyboard may do on a row: a folder opens; a link or a submodule is never followed.
const openable = (e: GitTreeEntry) => e.kind === "dir";

// FolderBrowser walks the folders of a repository at a ref and chooses the stack's folder. The
// list is a plain list of buttons with roving focus, not an ARIA listbox: Up and Down move, Enter
// or Right opens a folder, Left or Backspace goes up. Names come from a git host and are drawn as
// text, and never become anything but an encoded query parameter.
export function FolderBrowser({
  envId,
  repoId,
  repoName,
  kind,
  gitRef,
  folder,
  onUse,
}: FolderBrowserProps) {
  const t = useT();
  const ids = useId();
  const reason = useBrowseReason();
  const [path, setPath] = useState("");
  const [filter, setFilter] = useState("");
  const [active, setActive] = useState(0);
  // Once a folder is chosen the browser folds away to the line that names it; Change opens it.
  const [editing, setEditing] = useState(false);
  // The folder just entered, until its entries have arrived and been announced.
  const [entered, setEntered] = useState<string | null>(null);
  const [announcement, setAnnouncement] = useState("");
  const moveFocus = useRef(false);
  const list = useRef<HTMLUListElement>(null);
  const useButton = useRef<HTMLButtonElement>(null);
  const tree = useQuery(gitTreeQuery(envId, repoId, kind, gitRef, path));
  const data = tree.data?.path === path ? tree.data : undefined;

  const all = data?.entries ?? [];
  const needle = filter.trim().toLowerCase();
  const entries = needle ? all.filter((e) => e.name.toLowerCase().includes(needle)) : all;
  const segments = path ? path.split("/") : [];

  // When the folder just entered has arrived: say how many entries it holds, and put focus on its
  // first row, so a keyboard user is not left on a button that is gone.
  useEffect(() => {
    if (!data) return;
    if (entered !== null) {
      setAnnouncement(t("gitSource.folder.opened", { folder: entered, count: data.total }));
      setEntered(null);
    }
    if (moveFocus.current) {
      moveFocus.current = false;
      (list.current?.querySelector<HTMLElement>("button[data-row]") ?? useButton.current)?.focus();
    }
  }, [data, entered, t]);

  function go(next: string, name: string | null) {
    setPath(next);
    setFilter("");
    setActive(0);
    setEntered(name);
    if (name === null) setAnnouncement("");
    moveFocus.current = true;
  }
  const enter = (name: string) => go(join(path, name), name);
  const up = () => {
    if (path !== "") go(parentOf(path), null);
  };

  function onKeyDown(e: KeyboardEvent<HTMLUListElement>) {
    const row = (e.target as HTMLElement).closest<HTMLButtonElement>("button[data-row]");
    if (!row) return;
    const rows = Array.from(
      list.current?.querySelectorAll<HTMLButtonElement>("button[data-row]") ?? [],
    );
    const at = rows.indexOf(row);
    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        rows[Math.min(at + 1, rows.length - 1)]?.focus();
        return;
      case "ArrowUp":
        e.preventDefault();
        rows[Math.max(at - 1, 0)]?.focus();
        return;
      case "Home":
        e.preventDefault();
        rows[0]?.focus();
        return;
      case "End":
        e.preventDefault();
        rows[rows.length - 1]?.focus();
        return;
      case "ArrowRight":
        if (row.dataset.kind === "dir" && row.dataset.name) {
          e.preventDefault();
          enter(row.dataset.name);
        }
        return;
      case "ArrowLeft":
      case "Backspace":
        e.preventDefault();
        up();
        return;
    }
  }

  const chosenLine =
    folder === null ? null : (
      <div className="flex flex-wrap items-center gap-2">
        <p className="min-w-0 font-mono text-sm break-all text-muted-foreground">
          {folder === ""
            ? t("gitSource.folder.chosenRoot")
            : t("gitSource.folder.chosen", { path: folder })}
        </p>
        {editing ? null : (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="max-md:h-11"
            onClick={() => setEditing(true)}
          >
            {t("gitSource.folder.change")}
          </Button>
        )}
      </div>
    );

  if (folder !== null && !editing) return chosenLine;

  const hasInteractive = entries.some((e) => e.kind !== "file");
  const noCompose =
    data !== undefined &&
    all.length > 0 &&
    !all.some((e) => e.hasCompose || (e.kind === "file" && isYaml(e.name)));
  let rowIndex = -1;

  return (
    <div className="flex flex-col gap-3">
      <nav aria-label={t("gitSource.folder.pathBar")}>
        <ol className="flex flex-wrap items-center gap-1 text-sm">
          <li className="min-w-0">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              aria-label={t("gitSource.folder.root")}
              aria-current={path === "" ? "page" : undefined}
              className="max-w-full max-md:h-11"
              onClick={() => path !== "" && go("", null)}
            >
              <span className="break-all">{repoName}</span>
            </Button>
          </li>
          {segments.map((s, i) => {
            const to = segments.slice(0, i + 1).join("/");
            return (
              <li key={to} className="flex min-w-0 items-center gap-1">
                <span aria-hidden="true" className="text-muted-foreground">
                  /
                </span>
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  aria-current={to === path ? "page" : undefined}
                  className="max-w-full max-md:h-11"
                  onClick={() => to !== path && go(to, null)}
                >
                  <span className="break-all">{s}</span>
                </Button>
              </li>
            );
          })}
        </ol>
      </nav>

      <div className="flex flex-wrap items-center gap-2">
        <Button
          ref={useButton}
          type="button"
          variant="outline"
          className="max-md:h-11"
          disabled={!data}
          onClick={() => {
            onUse(path);
            setEditing(false);
          }}
        >
          {t("gitSource.folder.use")}
        </Button>
        {data && data.total >= FILTER_FROM ? (
          <Input
            aria-label={t("gitSource.folder.filter")}
            placeholder={t("gitSource.folder.filter")}
            value={filter}
            onChange={(e) => {
              setFilter(e.target.value);
              setActive(0);
            }}
            autoComplete="off"
            spellCheck={false}
            className="max-w-72 max-md:h-11"
          />
        ) : null}
      </div>
      {chosenLine}

      <div role="status" aria-live="polite" className="sr-only">
        {announcement}
      </div>

      {tree.isError ? (
        <Alert variant="destructive" role="alert">
          <CircleX aria-hidden="true" />
          <AlertDescription className="flex flex-col items-start gap-2 break-words">
            <span className="font-medium">
              {t("gitSource.folder.failed", { repo: repoName, ref: gitRef })}
            </span>
            <span>
              {reason(
                tree.error instanceof GitRequestError ? tree.error : new GitRequestError(0),
                repoName,
              )}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={tree.isFetching}
              onClick={() => void tree.refetch()}
            >
              {t("gitSource.folder.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : !data ? (
        <div className="flex flex-col gap-2" aria-busy="true">
          {Array.from({ length: SKELETON_ROWS }, (_, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: placeholder rows have no identity
            <Skeleton key={i} className="h-9 w-full" />
          ))}
          <p className="text-sm text-muted-foreground">
            {t("gitSource.folder.loading", { repo: repoName, ref: gitRef })}
          </p>
        </div>
      ) : (
        <>
          {all.length === 0 ? (
            <p className="text-sm text-muted-foreground">{t("gitSource.folder.empty")}</p>
          ) : null}
          {noCompose ? (
            <div className="flex flex-col gap-0.5 rounded-lg border border-dashed p-3 text-sm">
              <p className="font-medium">{t("gitSource.folder.noComposeTitle")}</p>
              <p className="text-muted-foreground">{t("gitSource.folder.noComposeBody")}</p>
            </div>
          ) : null}
          {all.length > 0 && entries.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              {t("gitSource.folder.noMatch", { query: filter.trim() })}
            </p>
          ) : null}
          {entries.length > 0 ? (
            <ul
              ref={list}
              aria-label={t("gitSource.folder.list")}
              onKeyDown={onKeyDown}
              className="max-h-80 overflow-y-auto"
            >
              {entries.map((e) => {
                if (e.kind === "file") {
                  return (
                    <li
                      key={e.name}
                      className="flex min-h-8 items-center gap-2 px-2.5 py-1 text-muted-foreground max-md:min-h-11"
                    >
                      <FileText aria-hidden="true" className="size-4 shrink-0" />
                      <TruncatedText className="max-w-full min-w-0 flex-1">{e.name}</TruncatedText>
                    </li>
                  );
                }
                rowIndex += 1;
                const mine = rowIndex;
                const full = join(path, e.name);
                const blocked = !openable(e);
                const describedBy =
                  e.kind === "submodule"
                    ? `${ids}-submodule`
                    : e.kind === "symlink"
                      ? `${ids}-link`
                      : undefined;
                const row = (
                  <Button
                    type="button"
                    variant="ghost"
                    data-row=""
                    data-kind={e.kind}
                    data-name={e.name}
                    tabIndex={mine === active ? 0 : -1}
                    aria-current={!blocked && folder === full ? "true" : undefined}
                    aria-disabled={blocked || undefined}
                    aria-describedby={describedBy}
                    onFocus={() => setActive(mine)}
                    onClick={() => !blocked && enter(e.name)}
                    className={cn(
                      "w-full justify-start gap-2 max-md:h-11",
                      blocked && "cursor-not-allowed opacity-70",
                    )}
                  >
                    <Folder aria-hidden="true" className={blocked ? "invisible" : undefined} />
                    <TruncatedText
                      className={cn(
                        "max-w-full min-w-0 flex-1 text-start",
                        !blocked && "font-semibold",
                      )}
                    >
                      {e.name}
                    </TruncatedText>
                    {e.hasCompose ? (
                      <Badge variant="outline">{t("gitSource.folder.hasCompose")}</Badge>
                    ) : null}
                    {e.kind === "submodule" ? (
                      <Badge variant="outline">{t("gitSource.folder.submodule")}</Badge>
                    ) : null}
                    {e.kind === "symlink" ? (
                      <Badge variant="outline">{t("gitSource.folder.link")}</Badge>
                    ) : null}
                    {blocked ? null : <ChevronRight aria-hidden="true" className="ms-auto" />}
                  </Button>
                );
                return (
                  <li key={e.name}>
                    {blocked ? (
                      <Tooltip>
                        <TooltipTrigger asChild>{row}</TooltipTrigger>
                        <TooltipContent>
                          {t(
                            e.kind === "submodule"
                              ? "gitSource.folder.submoduleTip"
                              : "gitSource.folder.linkTip",
                          )}
                        </TooltipContent>
                      </Tooltip>
                    ) : (
                      row
                    )}
                  </li>
                );
              })}
            </ul>
          ) : null}
          {hasInteractive ? (
            <div className="sr-only">
              <span id={`${ids}-submodule`}>{t("gitSource.folder.submoduleTip")}</span>
              <span id={`${ids}-link`}>{t("gitSource.folder.linkTip")}</span>
            </div>
          ) : null}
          {data.capped ? (
            <p className="text-sm text-muted-foreground">
              {t("gitSource.folder.capped", {
                shown: formatCount(all.length),
                n: formatCount(data.total),
              })}
            </p>
          ) : null}
        </>
      )}
    </div>
  );
}
