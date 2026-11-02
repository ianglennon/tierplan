// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { EllipsisVertical } from "lucide-react";
import { useState } from "react";

import { type EffectiveEnv, type Grant, teamGrantsQuery, userAccessQuery } from "@/api/grants";
import { teamQuery, teamsQuery } from "@/api/teams";
import { userQuery } from "@/api/users";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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
import { useT } from "@/i18n/use-t";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// Six skeleton rows hold the place of a table while it loads (UI-SPEC "loading").
const SKELETON_ROWS = 6;

// The tables scroll inside their card when a subject holds many environments
// (UI-SPEC "overflow: 100 environments"), and sideways under 768px.
const SCROLL = "max-h-96 overflow-auto rounded-md ring-1 ring-foreground/10";

function EnvironmentCell({ grant }: { grant: Grant }) {
  const t = useT();
  return grant.scope === "all" ? (
    <span>{t("grants.scope.all")}</span>
  ) : (
    <span className="block max-w-64 truncate" title={grant.envName}>
      {grant.envName}
    </span>
  );
}

// EffectiveTable is the "why" view (D-06): one row per environment the user can
// reach, with the highest role, the union of the extras and every source.
function EffectiveTable({ username, rows }: { username: string; rows: readonly EffectiveEnv[] }) {
  const t = useT();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("grants.card.effectiveLabel", { username })}>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow>
            <TableHead>{t("grants.card.columns.environment")}</TableHead>
            <TableHead>{t("grants.card.columns.role")}</TableHead>
            <TableHead>{t("grants.card.columns.extras")}</TableHead>
            <TableHead>{t("grants.card.columns.source")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((r) => (
            <TableRow key={r.envId}>
              <TableCell className="align-top">
                <span className="block max-w-64 truncate" title={r.envName}>
                  {r.envName}
                </span>
                {r.inherited ? (
                  <span className="block text-xs text-muted-foreground">
                    {t("grants.card.inherited")}
                  </span>
                ) : null}
              </TableCell>
              <TableCell className="align-top">
                <RoleBadge role={r.role} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <ExtraPermissionChips extras={r.extras ?? []} />
              </TableCell>
              <TableCell className="align-top whitespace-normal">
                <SourceBadges sources={r.sources ?? []} />
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

// AdminSummary is the one row an instance admin shows instead of a row per
// environment: they hold every environment, with every extra permission.
function AdminSummary({ username }: { username: string }) {
  const t = useT();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("grants.card.effectiveLabel", { username })}>
        <TableBody>
          <TableRow>
            <TableCell className="whitespace-normal">{t("grants.card.adminSummary")}</TableCell>
            <TableCell className="whitespace-normal">
              <SourceBadges sources={[{ kind: "instance" }]} />
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
    </div>
  );
}

// DirectTable lists the subject's own grants, the ones editable here. Each row
// has an "Actions for {environment}" menu with Edit and "Remove access…".
function DirectTable({
  subject,
  grants,
  onEdit,
  onRemove,
}: {
  subject: GrantSubject;
  grants: readonly Grant[];
  onEdit: (grant: Grant) => void;
  onRemove: (grant: Grant) => void;
}) {
  const t = useT();
  return (
    <div className={SCROLL}>
      <Table aria-label={t("grants.card.direct.label", { subject: subject.name })}>
        <TableHeader className="sticky top-0 z-10 bg-muted">
          <TableRow>
            <TableHead>{t("grants.card.columns.environment")}</TableHead>
            <TableHead>{t("grants.card.columns.role")}</TableHead>
            <TableHead>{t("grants.card.columns.extras")}</TableHead>
            <TableHead className="w-12">
              <span className="sr-only">{t("grants.card.columns.actions")}</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {grants.map((g) => {
            const where = g.scope === "all" ? t("grants.scope.all") : (g.envName ?? "");
            return (
              <TableRow key={g.id}>
                <TableCell className="align-top">
                  <EnvironmentCell grant={g} />
                </TableCell>
                <TableCell className="align-top">
                  <RoleBadge role={g.role} />
                </TableCell>
                <TableCell className="align-top whitespace-normal">
                  <ExtraPermissionChips extras={g.extras ?? []} />
                </TableCell>
                <TableCell className="align-top">
                  <DropdownMenu>
                    <DropdownMenuTrigger asChild>
                      <Button
                        type="button"
                        variant="ghost"
                        size="icon-sm"
                        className="max-md:size-11"
                        aria-label={t("grants.card.direct.actionsFor", { environment: where })}
                      >
                        <EllipsisVertical aria-hidden="true" />
                      </Button>
                    </DropdownMenuTrigger>
                    <DropdownMenuContent align="end">
                      <DropdownMenuItem onSelect={() => onEdit(g)}>
                        {t("grants.card.direct.edit")}
                      </DropdownMenuItem>
                      <DropdownMenuItem variant="destructive" onSelect={() => onRemove(g)}>
                        {t("grants.card.direct.remove")}
                      </DropdownMenuItem>
                    </DropdownMenuContent>
                  </DropdownMenu>
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}

function CardSkeleton() {
  const t = useT();
  return (
    <div aria-busy="true" className="flex flex-col gap-1.5">
      <span role="status" className="sr-only">
        {t("grants.card.loading")}
      </span>
      {Array.from({ length: SKELETON_ROWS }, (_, i) => (
        // Placeholder rows have no identity of their own.
        // biome-ignore lint/suspicious/noArrayIndexKey: static skeleton rows
        <Skeleton key={i} className="h-9 w-full" aria-hidden="true" />
      ))}
    </div>
  );
}

interface AccessCardProps {
  subject: GrantSubject;
}

// AccessCard is the Access section of a user's page and of a team's page (D-25).
// For a user it is the "why" view: a table of environments with the effective
// role, the extra permissions and where each comes from, an instance admin's
// single summary row, and below it "Direct access", the grants made to this
// user, which are the ones editable here. For a team it lists the team's grants.
// Add access opens the GrantDialog.
export function AccessCard({ subject }: AccessCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const isUser = subject.type === "user";
  const userAccess = useQuery({ ...userAccessQuery(subject.id), enabled: isUser });
  const teamGrants = useQuery({ ...teamGrantsQuery(subject.id), enabled: !isUser });
  const query = isUser ? userAccess : teamGrants;
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Grant | null>(null);
  const [removing, setRemoving] = useState<Grant | null>(null);

  const grants: readonly Grant[] = isUser
    ? (userAccess.data?.grants ?? [])
    : (teamGrants.data ?? []);
  const rows = userAccess.data?.environments ?? [];
  const instanceAdmin = isUser && userAccess.data?.instanceAdmin === true;

  // A saved or removed grant changes this card, the counts on the lists and, for
  // a user, who is reachable through their teams.
  function refresh() {
    const key = isUser
      ? userAccessQuery(subject.id).queryKey
      : teamGrantsQuery(subject.id).queryKey;
    void queryClient.invalidateQueries({ queryKey: key });
    if (isUser) {
      void queryClient.invalidateQueries({ queryKey: userQuery(subject.id).queryKey });
    } else {
      void queryClient.invalidateQueries({ queryKey: teamQuery(subject.id).queryKey });
      void queryClient.invalidateQueries({ queryKey: teamsQuery.queryKey });
    }
  }

  const empty = !instanceAdmin && (isUser ? rows.length === 0 : grants.length === 0);
  return (
    <SettingsCard title={isUser ? t("grants.card.title") : t("grants.card.teamTitle")} id="access">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="min-w-0 text-sm text-muted-foreground break-words">
          {isUser
            ? t("grants.card.caption", { username: subject.name })
            : t("grants.card.teamCaption", { team: subject.name })}
        </p>
        <Button
          type="button"
          variant="outline"
          className={`${TOUCH_HEIGHT} max-md:w-full`}
          onClick={() => setAdding(true)}
        >
          {t("grants.card.add")}
        </Button>
      </div>
      {query.isPending ? <CardSkeleton /> : null}
      {query.isError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription className="flex flex-wrap items-center gap-3">
            <span>
              {t("grants.card.loadFailed")} {t("grants.card.loadFailedBody")}
            </span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              disabled={query.isFetching}
              onClick={() => void query.refetch()}
            >
              {query.isFetching ? <Spinner aria-hidden="true" /> : null}
              {t("grants.card.retry")}
            </Button>
          </AlertDescription>
        </Alert>
      ) : null}
      {query.data ? (
        <>
          {instanceAdmin ? <AdminSummary username={subject.name} /> : null}
          {isUser && !instanceAdmin && rows.length > 0 ? (
            <EffectiveTable username={subject.name} rows={rows} />
          ) : null}
          {empty ? (
            <p className="text-sm text-muted-foreground">
              {isUser
                ? t("grants.card.empty", { username: subject.name })
                : t("grants.card.teamEmpty", { team: subject.name })}
            </p>
          ) : null}
          {grants.length > 0 ? (
            <section className="flex flex-col gap-2">
              {isUser ? (
                <h4 className="text-sm font-semibold">{t("grants.card.direct.title")}</h4>
              ) : null}
              <DirectTable
                subject={subject}
                grants={grants}
                onEdit={setEditing}
                onRemove={setRemoving}
              />
            </section>
          ) : null}
        </>
      ) : null}
      <GrantDialog
        subject={subject}
        open={adding || editing !== null}
        onOpenChange={(open) => {
          if (open) return;
          setAdding(false);
          setEditing(null);
        }}
        grant={editing}
        grants={grants}
        onSaved={refresh}
      />
      <RemoveGrantDialog
        subject={subject}
        grant={removing}
        onClose={() => setRemoving(null)}
        onDone={() => {
          setRemoving(null);
          refresh();
        }}
        onStale={refresh}
      />
    </SettingsCard>
  );
}
