// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { Check, Copy } from "lucide-react";
import { useEffect, useRef, useState } from "react";

import type { StackRevision } from "@/api/stack-history";
import { copyText } from "@/components/copyable-code";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableRow,
} from "@/components/ui/table";
import { shortSha } from "@/features/stack-git/sha-link";
import { useT } from "@/i18n/use-t";
import { toastError } from "@/lib/toast";

// How long the Copy button shows that it copied.
const COPIED_MS = 2000;

// CopyCommit is the full commit ID in mono with a button that copies it. The result is told in words for a screen
// reader and by the icon for everyone else.
function CopyCommit({ sha }: { sha: string }) {
  const t = useT();
  const [copied, setCopied] = useState(false);
  const host = useRef<HTMLSpanElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);

  async function onCopy() {
    clearTimeout(timer.current);
    if (await copyText(sha, host.current)) {
      setCopied(true);
      timer.current = setTimeout(() => setCopied(false), COPIED_MS);
      return;
    }
    toastError(t("env.copy.failed"));
  }

  return (
    <span ref={host} className="flex min-w-0 items-start gap-1">
      <code className="min-w-0 font-mono text-xs break-all">{sha}</code>
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        className="-my-1 shrink-0 max-md:size-11"
        aria-label={t("stackRevision.contents.source.copy")}
        onClick={() => void onCopy()}
      >
        {copied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
      </Button>
      <span role="status" className="sr-only">
        {copied ? t("stackRevision.contents.source.copied") : ""}
      </span>
    </span>
  );
}

// RevisionSource is the Source table of a git revision's Contents (UI-SPEC "History additions", D-08): the repository,
// ref, commit, the hash of the folder's tree and the compose files it merged, all as stored with the revision. The
// helper says a roll back never needs the repository. Every value is the repository's or the server's text, drawn as
// text.
export function RevisionSource({ item }: { item: StackRevision }) {
  const t = useT();
  const src = item.source;
  const files = src?.composeFiles ?? [];
  return (
    <div className="flex min-w-0 flex-col gap-2">
      <div className="rounded-md border">
        <Table>
          <TableCaption className="sr-only">
            {t("stackRevision.contents.source.caption", { seq: item.seq })}
          </TableCaption>
          <TableBody>
            <TableRow>
              <TableHead scope="row">{t("stackRevision.contents.source.repository")}</TableHead>
              <TableCell className="break-all whitespace-normal">{src?.repoName ?? ""}</TableCell>
            </TableRow>
            <TableRow>
              <TableHead scope="row">{t("stackRevision.contents.source.ref")}</TableHead>
              <TableCell className="font-mono text-xs break-all whitespace-normal">
                {src?.ref ?? ""}
              </TableCell>
            </TableRow>
            <TableRow>
              <TableHead scope="row">{t("stackRevision.contents.source.commit")}</TableHead>
              <TableCell className="whitespace-normal">
                <CopyCommit sha={item.commit} />
              </TableCell>
            </TableRow>
            <TableRow>
              <TableHead scope="row">{t("stackRevision.contents.source.tree")}</TableHead>
              <TableCell className="font-mono text-xs">{shortSha(item.tree)}</TableCell>
            </TableRow>
            <TableRow>
              <TableHead scope="row">{t("stackRevision.contents.source.folder")}</TableHead>
              <TableCell className="font-mono text-xs break-all whitespace-normal">
                {src?.folder || t("stackGit.details.rootFolder")}
              </TableCell>
            </TableRow>
            <TableRow>
              <TableHead scope="row">{t("stackRevision.contents.source.composeFiles")}</TableHead>
              <TableCell className="whitespace-normal">
                <ol className="flex flex-col gap-0.5 font-mono text-xs break-all">
                  {files.map((f) => (
                    <li key={f}>{f}</li>
                  ))}
                </ol>
              </TableCell>
            </TableRow>
          </TableBody>
        </Table>
      </div>
      <p className="text-sm text-muted-foreground">{t("stackRevision.contents.source.helper")}</p>
    </div>
  );
}
