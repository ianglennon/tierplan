// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { ChevronRight } from "lucide-react";
import { useState } from "react";

import { stackGitCommitsQuery } from "@/api/stack-git";
import { RelativeTime } from "@/components/relative-time";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { TruncatedText } from "@/features/resources/resource-table";
import { ShaLink } from "@/features/stack-git/sha-link";
import { useT } from "@/i18n/use-t";
import { cn } from "@/lib/utils";

// The card is open by default when this many commits or fewer are listed, and lists this many rows before "Show all"
// (UI-SPEC "Review additions").
const OPEN_BY_DEFAULT = 5;
const ROWS_SHOWN = 10;

interface CommitsCardProps {
  envId: string;
  stackId: string;
  // The commit the review deploys: the commits are those up to it that changed the folder since the deployed one.
  head: string;
  // The stack's folder, "" for the root of the repository.
  folder: string;
  // The host and path of the saved repository address, which decide whether a commit ID is a link.
  repoHost: string;
  repoPath: string;
}

// CommitsCard is the review's list of what changed in the repository (UI-SPEC "Review additions", D-13): the commits
// that changed the stack's folder between the deployed commit and the one reviewed, newest first. It informs and never
// gates a deploy, so a list the server can't make is one sentence and the review goes on. Subjects and authors are
// repository text and are drawn as text: one line with a Tooltip in the table, wrapped when every commit is shown.
// The caller leaves it out for a first deploy and for a change of source, which have no commits to compare.
export function CommitsCard({
  envId,
  stackId,
  head,
  folder,
  repoHost,
  repoPath,
}: CommitsCardProps) {
  const t = useT();
  const commits = useQuery(stackGitCommitsQuery(envId, stackId, head));
  const [opened, setOpened] = useState<boolean | undefined>(undefined);
  const [all, setAll] = useState(false);
  const data = commits.data;
  const list = data?.commits ?? [];
  const open = opened ?? list.length <= OPEN_BY_DEFAULT;
  const where = folder || t("stackGit.status.rootFolder");
  return (
    <Card
      data-slot="commits-card"
      className="gap-3 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]"
    >
      <Collapsible open={open} onOpenChange={setOpened}>
        <CardHeader>
          <h3 className="text-base font-semibold">
            <CollapsibleTrigger className="flex items-center gap-2 rounded-sm text-start focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none max-md:min-h-11">
              <ChevronRight
                aria-hidden="true"
                className={cn("size-4 shrink-0 transition-transform", open && "rotate-90")}
              />
              {t("stackReview.git.commits.title")}
            </CollapsibleTrigger>
          </h3>
        </CardHeader>
        <CollapsibleContent>
          <CardContent className="flex flex-col gap-3 pt-3">
            {commits.isPending ? (
              <div data-testid="commits-skeleton" className="flex flex-col gap-2">
                <div aria-hidden="true" className="flex flex-col gap-2">
                  <Skeleton className="h-9 w-full" />
                  <Skeleton className="h-9 w-full" />
                  <Skeleton className="h-9 w-full" />
                </div>
                <p role="status" className="text-sm text-muted-foreground">
                  {t("stackReview.git.commits.loading")}
                </p>
              </div>
            ) : !data?.available ? (
              <p className="text-sm text-muted-foreground">
                {t("stackReview.git.commits.unavailable")}
              </p>
            ) : (
              <>
                <Table>
                  <caption className="sr-only">
                    {t("stackReview.git.commits.caption", { folder: where })}
                  </caption>
                  <TableHeader>
                    <TableRow>
                      <TableHead scope="col">{t("stackReview.git.commits.columns.sha")}</TableHead>
                      <TableHead scope="col">
                        {t("stackReview.git.commits.columns.subject")}
                      </TableHead>
                      <TableHead scope="col">
                        {t("stackReview.git.commits.columns.author")}
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {(all ? list : list.slice(0, ROWS_SHOWN)).map((c) => (
                      <TableRow key={c.sha}>
                        <TableCell className="align-top text-xs whitespace-normal">
                          <ShaLink sha={c.sha} repoHost={repoHost} repoPath={repoPath} />
                        </TableCell>
                        <TableCell className="align-top text-sm whitespace-normal">
                          {all ? (
                            <span className="break-words">{c.subject}</span>
                          ) : (
                            <TruncatedText className="max-w-md">{c.subject}</TruncatedText>
                          )}
                        </TableCell>
                        <TableCell className="align-top text-xs whitespace-normal text-muted-foreground">
                          <span className="break-words">
                            {t("stackReview.git.commits.by", { author: c.author })}
                          </span>
                          <RelativeTime value={c.when} />
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
                {list.length > ROWS_SHOWN ? (
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="self-start max-md:h-11"
                    aria-expanded={all}
                    onClick={() => setAll(!all)}
                  >
                    {all
                      ? t("stackReview.git.commits.showFewer")
                      : t("stackReview.git.commits.showAll", { count: list.length })}
                  </Button>
                ) : null}
                {data.total > list.length || data.capped ? (
                  <p className="text-xs text-muted-foreground">
                    {data.capped
                      ? t("stackReview.git.commits.capped", { total: data.total })
                      : t("stackReview.git.commits.partial", {
                          shown: list.length,
                          total: data.total,
                        })}
                  </p>
                ) : null}
              </>
            )}
          </CardContent>
        </CollapsibleContent>
      </Collapsible>
    </Card>
  );
}
