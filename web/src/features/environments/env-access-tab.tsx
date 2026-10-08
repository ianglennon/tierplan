// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ChevronDown, ChevronRight, EllipsisVertical, Lock } from "lucide-react";
import { Fragment, useEffect, useId, useState } from "react";

import {
  type EnvAccessPerson,
  type EnvAccessTeam,
  envAccessCandidatesQuery,
  envAccessQuery,
} from "@/api/envaccess";
import { environmentsQuery } from "@/api/environments";
import type { Grant, SubjectType } from "@/api/grants";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ExtraPermissionChips } from "@/features/access/extra-permission-chips";
import { GrantDialog, type GrantSubject } from "@/features/access/grant-dialog";
import { RemoveGrantDialog } from "@/features/access/remove-grant-dialog";
import { RoleBadge } from "@/features/access/role-badge";
import { SourceBadges } from "@/features/access/source-badges";
import { SettingsCard } from "@/features/environments/settings-card";
import { meQueryOptions, useMe, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Six skeleton rows hold the place of a table while it loads (UI-SPEC "loading").
const SKELETON_ROWS = 6;

// Both tables scroll inside their card however many people there are (UI-SPEC
// "overflow: 100 people"), and sideways under 768px.
const SCROLL = "max-h-96 overflow-auto rounded-md ring-1 ring-foreground/10";

const NONE: readonly never[] = [];

// The subject of a removal dialog that is closed.
const NO_SUBJECT: GrantSubject = { type: "user", id: "", name: "" };

// Admin first, then Operator, then Viewer (UI-SPEC "Environment Access tab").
const ROLE_RANK: Record<string, number> = { admin: 0, operator: 1, viewer: 2 };

function byRoleThenName<T>(role: (x: T) => string, name: (x: T) => string) {
  return (a: T, b: T) =>
    (ROLE_RANK[role(a)] ?? 9) - (ROLE_RANK[role(b)] ?? 9) ||
    name(a).localeCompare(name(b), undefined, { sensitivity: "base" });
}

const has = (text: string | undefined, needle: string) =>
  (text ?? "").toLowerCase().includes(needle);

// personMatches: the search matches a person's username or display name.
const personMatches = (p: { username: string; displayName?: string }, needle: string) =>
  needle === "" || has(p.username, needle) || has(p.displayName, needle);

// membersMatch says whether the search matches one of a team's members.
const membersMatch = (x: EnvAccessTeam, needle: string) =>
  needle !== "" && (x.members ?? NONE).some((m) => personMatches(m, needle));

// A row the signed-in user may edit holds a grant of its own on this
// environment; the others come from every environment, a team or the instance,
// which only an instance admin can change.
interface Target {
  subject: GrantSubject;
  grant: Grant;
}

function grantOf(
  type: SubjectType,
  id: string,
  ref: { id: string; role: string; extras?: string[] | null },
  envId: string,
  envName: string,
): Grant {
  return {
    id: ref.id,
    subjectType: type,
    subjectId: id,
    scope: "env",
    envId,
    envName,
    role: ref.role as Grant["role"],
    extras: ref.extras ?? [],
    createdAt: "",
  };
}

interface RowActionsProps {
  name: string;
  // The row's own grant here, or undefined for a row that is read only.
  target?: Target;
  onEdit: (target: Target) => void;
  onRemove: (target: Target) => void;
}

// RowActions is the row menu "Edit access" / "Remove access…" for a row with a
// grant of its own on this environment. Any other row shows a lock with the
// reason in place of the menu (UI-SPEC "Read-only rows"): the control stays in
// the tab order and the reason is also its description.
function RowActions({ name, target, onEdit, onRemove }: RowActionsProps) {
  const t = useT();
  const label = t("envaccess.tab.actionsFor", { name });
  if (!target) {
    return (
      <NeedsTooltip reason={t("envaccess.tab.readOnly")}>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={label}
        >
          <Lock aria-hidden="true" />
        </Button>
      </NeedsTooltip>
    );
  }
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon-sm"
          className="max-md:size-11"
          aria-label={label}
        >
          <EllipsisVertical aria-hidden="true" />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        <DropdownMenuItem onSelect={() => onEdit(target)}>
          {t("envaccess.tab.edit")}
        </DropdownMenuItem>
        <DropdownMenuItem variant="destructive" onSelect={() => onRemove(target)}>
          {t("envaccess.tab.remove")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

interface TablesProps {
  envId: string;
  envName: string;
  onEdit: (target: Target) => void;
  onRemove: (target: Target) => void;
}

function TeamsTable({
  envId,
  envName,
  teams,
  expanded,
  onToggle,
  onEdit,
  onRemove,
}: TablesProps & {
  teams: readonly EnvAccessTeam[];
  expanded: ReadonlySet<string>;
  onToggle: (teamId: string) => void;
}) {
  const t = useT();
  const panels = useId();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("envaccess.teams.label", { name: envName })}>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow>
            <TableHead>{t("envaccess.teams.columns.team")}</TableHead>
            <TableHead>{t("envaccess.teams.columns.role")}</TableHead>
            <TableHead>{t("envaccess.teams.columns.extras")}</TableHead>
            <TableHead>{t("envaccess.teams.columns.source")}</TableHead>
            <TableHead className="w-12">
              <span className="sr-only">{t("envaccess.teams.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {teams.map((x) => {
            const open = expanded.has(x.teamId);
            const panel = `${panels}-${x.teamId}`;
            const members = x.members ?? NONE;
            return (
              <Fragment key={x.teamId}>
                <TableRow>
                  <TableCell className="align-top">
                    <div className="flex items-center gap-1">
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon-sm"
                        className="max-md:size-11"
                        aria-expanded={open}
                        aria-controls={panel}
                        aria-label={t(open ? "envaccess.teams.hide" : "envaccess.teams.show", {
                          name: x.name,
                        })}
                        onClick={() => onToggle(x.teamId)}
                      >
                        {open ? (
                          <ChevronDown aria-hidden="true" />
                        ) : (
                          <ChevronRight aria-hidden="true" />
                        )}
                      </Button>
                      <span className="block max-w-56 truncate" title={x.name}>
                        {x.name}
                      </span>
                    </div>
                  </TableCell>
                  <TableCell className="align-top">
                    <RoleBadge role={x.role} />
                  </TableCell>
                  <TableCell className="align-top whitespace-normal">
                    {x.role === "admin" ? (
                      <span>{t("envaccess.tab.all")}</span>
                    ) : (
                      <ExtraPermissionChips extras={x.extras ?? []} />
                    )}
                  </TableCell>
                  <TableCell className="align-top whitespace-normal">
                    <SourceBadges sources={x.sources ?? []} linkTeams={false} />
                  </TableCell>
                  <TableCell className="align-top">
                    <RowActions
                      name={x.name}
                      target={
                        x.grant
                          ? {
                              subject: { type: "team", id: x.teamId, name: x.name },
                              grant: grantOf("team", x.teamId, x.grant, envId, envName),
                            }
                          : undefined
                      }
                      onEdit={onEdit}
                      onRemove={onRemove}
                    />
                  </TableCell>
                </TableRow>
                {open ? (
                  <TableRow className="hover:bg-transparent">
                    <TableCell id={panel} colSpan={5} className="bg-muted/30 whitespace-normal">
                      {members.length === 0 ? (
                        <p className="text-sm text-muted-foreground">
                          {t("envaccess.teams.noMembers")}
                        </p>
                      ) : (
                        <ul
                          aria-label={t("envaccess.teams.members", { name: x.name })}
                          className="flex flex-col gap-1.5"
                        >
                          {members.map((m) => (
                            <li
                              key={m.userId}
                              className="flex min-w-0 flex-wrap items-center gap-2"
                            >
                              <span className="max-w-56 truncate" title={m.username}>
                                {m.username}
                              </span>
                              {m.displayName ? (
                                <span className="max-w-56 truncate text-xs text-muted-foreground">
                                  {m.displayName}
                                </span>
                              ) : null}
                              <RoleBadge role={m.role} />
                            </li>
                          ))}
                        </ul>
                      )}
                    </TableCell>
                  </TableRow>
                ) : null}
              </Fragment>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

function PeopleTable({
  envId,
  envName,
  people,
  onEdit,
  onRemove,
}: TablesProps & { people: readonly EnvAccessPerson[] }) {
  const t = useT();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("envaccess.people.label", { name: envName })}>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow>
            <TableHead>{t("envaccess.people.columns.user")}</TableHead>
            <TableHead>{t("envaccess.people.columns.role")}</TableHead>
            <TableHead>{t("envaccess.people.columns.extras")}</TableHead>
            <TableHead>{t("envaccess.people.columns.source")}</TableHead>
            <TableHead className="w-12">
              <span className="sr-only">{t("envaccess.people.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {people.map((p) => (
            <TableRow key={p.userId}>
              <TableCell className="align-top">
                <span className="block max-w-64 truncate" title={p.username}>
                  {p.username}
                  {p.self ? (
                    <span className="text-muted-foreground"> {t("envaccess.tab.you")}</span>
                  ) : null}
                </span>
                {p.displayName ? (
                  <span className="block max-w-64 truncate text-xs text-muted-foreground">
                    {p.displayName}
                  </span>
                ) : null}
              </TableCell>
              <TableCell className="align-top">
                <RoleBadge role={p.role} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                {p.role === "admin" ? (
                  <span>{t("envaccess.tab.all")}</span>
                ) : (
                  <ExtraPermissionChips extras={p.extras ?? []} />
                )}
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <SourceBadges sources={p.sources ?? []} linkTeams={false} />
              </TableCell>
              <TableCell className="align-top">
                <RowActions
                  name={p.username}
                  target={
                    p.grant
                      ? {
                          subject: { type: "user", id: p.userId, name: p.username },
                          grant: grantOf("user", p.userId, p.grant, envId, envName),
                        }
                      : undefined
                  }
                  onEdit={onEdit}
                  onRemove={onRemove}
                />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function TabSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true" className="flex flex-col gap-1.5">
      <span role="status" className="sr-only">
        {t("envaccess.tab.loading")}
      </span>
      {Array.from({ length: SKELETON_ROWS }, (_, i) => (
        // Placeholder rows have no identity of their own.
        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
        <Skeleton key={i} className="h-9 w-full" aria-hidden="true" />
      ))}
    </div>
  );
}

interface EnvAccessTabProps {
  envId: string;
}

// EnvAccessTab is the Access tab of an environment's settings page (D-25, D-06):
// everyone who can use the environment and where their access comes from, in two
// tables, Teams and People, with the highest role, the union of the extra
// permissions and every source, sorted by role then name, with the signed-in
// user's own row marked "(you)". One search field filters both tables; a team
// row expands to its members, and a search that matches a member opens it. A row
// with a grant of its own on this environment has "Edit access" and "Remove
// access…"; every other row shows a lock, because only an instance admin can
// change what comes from every environment, a team or the instance itself.
// "Add access" opens the GrantDialog on this environment. The tab is for someone
// who holds environments.access here, an env Admin or an instance admin; the
// server answers 403 (or 404) otherwise.
export function EnvAccessTab({ envId }: EnvAccessTabProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const me = useMe();
  const canGive = usePermission("environments.access", envId);
  const environments = useQuery(environmentsQuery);
  const name = environments.data?.items?.find((e) => e.id === envId)?.name ?? "";
  const access = useQuery(envAccessQuery(envId));
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Target | null>(null);
  const [removing, setRemoving] = useState<Target | null>(null);
  const [query, setQuery] = useState("");
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());

  const teams = access.data?.teams ?? NONE;
  const people = access.data?.people ?? NONE;
  const needle = query.trim().toLowerCase();

  // A search that matches a member opens that team's row. The row can still be
  // closed by hand afterwards.
  useEffect(() => {
    if (needle === "") return;
    const open = teams.filter((x) => membersMatch(x, needle)).map((x) => x.teamId);
    if (open.length === 0) return;
    setExpanded((now) => (open.every((id) => now.has(id)) ? now : new Set([...now, ...open])));
  }, [needle, teams]);

  function toggle(teamId: string) {
    setExpanded((now) => {
      const next = new Set(now);
      if (!next.delete(teamId)) next.add(teamId);
      return next;
    });
  }

  // A saved or removed grant changes the tables and who the Who list offers. A
  // change to the signed-in user's own access changes what the app lets them see.
  function refresh(subjectId?: string) {
    void queryClient.invalidateQueries({ queryKey: envAccessQuery(envId).queryKey });
    void queryClient.invalidateQueries({ queryKey: envAccessCandidatesQuery(envId).queryKey });
    if (subjectId !== undefined && subjectId === me.data?.user.id) {
      void queryClient.invalidateQueries({ queryKey: meQueryOptions.queryKey });
    }
  }

  const shownTeams = teams
    .filter((x) => needle === "" || has(x.name, needle) || membersMatch(x, needle))
    .sort(
      byRoleThenName(
        (x) => x.role,
        (x) => x.name,
      ),
    );
  const shownPeople = people
    .filter((p) => personMatches(p, needle))
    .sort(
      byRoleThenName(
        (p) => p.role,
        (p) => p.username,
      ),
    );
  const environment = { id: envId, name };
  return (
    <SettingsCard title={t("envaccess.tab.title", { name })}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="min-w-0 text-sm text-muted-foreground break-words">
          {t("envaccess.tab.caption")}
        </p>
        {canGive ? (
          <Button
            type="button"
            className={`${TOUCH_HEIGHT} max-md:w-full`}
            onClick={() => setAdding(true)}
          >
            {t("envaccess.tab.add")}
          </Button>
        ) : null}
      </div>
      {access.isPending ? <TabSkeleton /> : null}
      {access.isError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>
              {t("envaccess.tab.loadFailed")} {t("envaccess.tab.loadFailedBody")}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={access.isFetching}
              onClick={() => void access.refetch()}
            >
              {access.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("envaccess.tab.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {access.data ? (
        <>
          <Input
            type="search"
            aria-label={t("envaccess.tab.search")}
            placeholder={t("envaccess.tab.search")}
            value={query}
            maxLength={128}
            autoComplete="off"
            className={`${TOUCH_HEIGHT} sm:max-w-sm`}
            onChange={(e) => setQuery(e.target.value)}
          />
          <section className="flex flex-col gap-2">
            <h4 className="text-sm font-semibold">{t("envaccess.teams.heading")}</h4>
            {teams.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                {t("envaccess.teams.empty", { name })}
              </p>
            ) : shownTeams.length === 0 ? (
              <p className="text-sm text-muted-foreground">
                {t("envaccess.teams.noMatch", { query: query.trim() })}
              </p>
            ) : (
              <TeamsTable
                envId={envId}
                envName={name}
                teams={shownTeams}
                expanded={expanded}
                onToggle={toggle}
                onEdit={setEditing}
                onRemove={setRemoving}
              />
            )}
          </section>
          <section className="flex flex-col gap-2">
            <h4 className="text-sm font-semibold">{t("envaccess.people.heading")}</h4>
            {shownPeople.length === 0 && needle !== "" ? (
              <p className="text-sm text-muted-foreground">
                {t("envaccess.people.noMatch", { query: query.trim() })}
              </p>
            ) : (
              <PeopleTable
                envId={envId}
                envName={name}
                people={shownPeople}
                onEdit={setEditing}
                onRemove={setRemoving}
              />
            )}
          </section>
        </>
      ) : null}
      <GrantDialog
        mode="env"
        environment={environment}
        subject={editing?.subject}
        grant={editing?.grant ?? null}
        open={adding || editing !== null}
        onOpenChange={(open) => {
          if (open) return;
          setAdding(false);
          setEditing(null);
        }}
        onSaved={() => refresh(editing?.subject.id)}
      />
      <RemoveGrantDialog
        subject={removing?.subject ?? NO_SUBJECT}
        environment={environment}
        grant={removing?.grant ?? null}
        onClose={() => setRemoving(null)}
        onDone={() => {
          const gone = removing?.subject.id;
          setRemoving(null);
          refresh(gone);
        }}
        onStale={() => refresh()}
      />
    </SettingsCard>
  );
}
