// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useRouter, useRouterState } from "@tanstack/react-router";
import { useEffect } from "react";

import { type UserDetail, UserNotFoundError, userQuery, usersQuery } from "@/api/users";
import { NoPermission } from "@/components/full-page-state";
import { NotFound } from "@/components/not-found";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { AccessCard } from "@/features/access/access-card";
import { AccountCard } from "@/features/access/account-card";
import { InstanceAdminBadge } from "@/features/access/instance-admin-badge";
import { InstanceAdminCard } from "@/features/access/instance-admin-card";
import { OwnedResourcesCard } from "@/features/access/owned-resources-card";
import { ProfileCard } from "@/features/access/profile-card";
import { SessionsCard } from "@/features/access/sessions-card";
import { TeamsCard } from "@/features/access/teams-card";
import { UserStatusBadge } from "@/features/access/user-status-badge";
import { useDocumentTitle, usePermission } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function Breadcrumb({ username }: { username: string }) {
  const t = useT();
  return (
    <nav aria-label={t("userpage.breadcrumb.label")}>
      <ol className="flex flex-wrap items-center gap-x-2 text-xs text-muted-foreground [&>li:not(:last-child)]:after:ms-2 [&>li:not(:last-child)]:after:content-['/']">
        <li>
          <Link to="/settings" className="rounded-sm text-accent-ink hover:underline">
            {t("userpage.breadcrumb.settings")}
          </Link>
        </li>
        <li>
          <Link to="/settings/access" className="rounded-sm text-accent-ink hover:underline">
            {t("userpage.breadcrumb.access")}
          </Link>
        </li>
        <li>
          <Link to="/settings/access/users" className="rounded-sm text-accent-ink hover:underline">
            {t("userpage.breadcrumb.users")}
          </Link>
        </li>
        <li className="min-w-0">
          <span aria-current="page" className="break-words">
            {username}
          </span>
        </li>
      </ol>
    </nav>
  );
}

// PageSkeleton holds the final layout while the user loads: the header and one
// block per card (UI-SPEC "loading: user page").
function PageSkeleton() {
  const t = useT();
  return (
    <div className="flex flex-col gap-8" aria-busy="true">
      <span role="status" className="sr-only">
        {t("userpage.loading")}
      </span>
      <div className="flex flex-col gap-2" aria-hidden="true">
        <Skeleton className="h-3 w-64" />
        <Skeleton className="h-8 w-48" />
      </div>
      <Skeleton className="h-32 w-full" aria-hidden="true" />
      <Skeleton className="h-32 w-full" aria-hidden="true" />
    </div>
  );
}

interface UserPageProps {
  userId: string;
}

// UserPage is Settings > Access > Users > {username}: the breadcrumb, the header
// (the display name as the title when there is one, else the username, with the
// status and the instance-admin badge) and the cards, 32px apart: Profile,
// Instance administrator, Access, Teams, Sessions, Owned automations (only when the user owns any) and Account. The page is
// for an instance admin, who holds users.manage; the server answers 403 again.
export function UserPage({ userId }: UserPageProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const router = useRouter();
  const allowed = usePermission("users.manage");
  const query = useQuery({ ...userQuery(userId), enabled: allowed });
  const user = query.data;
  useDocumentTitle(user?.username);
  // A link to a card ("Set up access" after creating a user) lands on it once the
  // page has loaded; the card does not exist while the skeleton shows.
  const hash = useRouterState({ select: (s) => s.location.hash });
  useEffect(() => {
    if (user && hash) document.getElementById(hash)?.scrollIntoView();
  }, [user, hash]);

  if (!allowed) return <NoPermission />;
  if (query.error instanceof UserNotFoundError) return <NotFound />;
  if (query.isError && !user) {
    return (
      <Alert variant="destructive">
        <AlertTitle>{t("userpage.loadFailed")}</AlertTitle>
        <AlertDescription className="flex flex-wrap items-center gap-3">
          <span>{t("userpage.loadFailedBody")}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="max-md:h-11"
            disabled={query.isFetching}
            onClick={() => void query.refetch()}
          >
            {query.isFetching ? <Spinner aria-hidden="true" /> : null}
            {t("userpage.retry")}
          </Button>
        </AlertDescription>
      </Alert>
    );
  }
  if (!user) return <PageSkeleton />;

  // A change answers with the user as they now are; showing that at once and
  // reading the list again keeps the page and the table in step.
  function changed(updated?: UserDetail) {
    if (updated) queryClient.setQueryData(userQuery(userId).queryKey, updated);
    void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });
  }

  // The user is gone: forget the page and go back to the list it came from.
  function deleted() {
    queryClient.removeQueries({ queryKey: userQuery(userId).queryKey });
    void queryClient.invalidateQueries({ queryKey: usersQuery.queryKey });
    void router.navigate({ to: "/settings/access/users" });
  }

  const title = user.displayName || user.username;
  return (
    <div className="flex flex-col gap-8">
      <header className="flex flex-col gap-2">
        <Breadcrumb username={user.username} />
        <div className="flex min-w-0 flex-col gap-1">
          <h2 className="min-w-0 text-2xl font-semibold break-words">{title}</h2>
          {user.displayName ? (
            <p className="text-sm break-all text-muted-foreground">{user.username}</p>
          ) : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <UserStatusBadge status={user.status} lockedUntil={user.lockedUntil} />
          {user.instanceAdmin ? <InstanceAdminBadge /> : null}
        </div>
      </header>
      <ProfileCard user={user} onChanged={changed} />
      <InstanceAdminCard user={user} onChanged={changed} />
      <AccessCard subject={{ type: "user", id: user.id, name: user.username }} />
      <TeamsCard user={user} />
      <SessionsCard userId={user.id} username={user.username} />
      <OwnedResourcesCard userId={user.id} username={user.username} />
      <AccountCard user={user} onChanged={changed} onDeleted={deleted} />
    </div>
  );
}
