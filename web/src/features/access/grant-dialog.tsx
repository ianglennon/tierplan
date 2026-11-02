// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { envAccessCandidatesQuery, previewEnvAccessImpact, putEnvAccess } from "@/api/envaccess";
import { environmentsQuery } from "@/api/environments";
import { type Grant, previewGrantImpact, putGrant, type SubjectType } from "@/api/grants";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Skeleton } from "@/components/ui/skeleton";
import { Spinner } from "@/components/ui/spinner";
import { ExtraPermissions } from "@/features/access/extra-permissions";
import { ImpactPreview } from "@/features/access/impact-preview";
import { type RoleId, RolePicker } from "@/features/access/role-picker";
import { type Who, WhoCombobox } from "@/features/access/who-combobox";
import { useInstanceAdmin } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";
import { toastSuccess } from "@/lib/toast";

// 44px targets under 768px (D-64), the dense 32px size from there up.
const TOUCH_HEIGHT = "h-11 md:h-8";

// The Select value of "All environments"; no environment has this ID.
const ALL = "*";

export interface GrantSubject {
  type: SubjectType;
  id: string;
  // The username or the team name, shown as text only.
  name: string;
}

// The environment an Access tab gives access on.
export interface GrantEnvironment {
  id: string;
  name: string;
}

interface GrantDialogBase {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // The grant being edited, or null to add one.
  grant?: Grant | null;
  // The subject's own grants, so choosing an environment they already hold can
  // say that saving replaces it. Only the user and team pages pass them.
  grants?: readonly Grant[];
  // Called once the grant is saved, before the dialog closes.
  onSaved: () => void;
}

// The two entry points (UI-SPEC "GrantDialog"): from a user's or a team's page,
// where the subject is known and the environment is chosen; and from an
// environment's Access tab (mode "env"), where the environment is known and the
// subject is chosen in the Who list. The Access tab gives access through the
// route an env Admin may use, which cannot reach any other environment.
export type GrantDialogProps = GrantDialogBase &
  (
    | { mode?: "subject"; subject: GrantSubject; environment?: undefined }
    | { mode: "env"; environment: GrantEnvironment; subject?: GrantSubject }
  );

// scopeOf is the Select value a grant is on.
function scopeOf(g: Grant): string {
  return g.scope === "all" ? ALL : (g.envId ?? "");
}

const RANK: Record<string, number> = { viewer: 1, operator: 2, admin: 3 };

// takesAwayFrom says whether saving role and extras in place of the grant being
// edited lowers it: a lower role, or fewer extras at the same role. An Admin holds
// every extra, so going up to Admin takes none away. Only a lowering is previewed
// (D-27); raising access never takes anything from anyone.
function takesAwayFrom(grant: Grant, role: string, extras: readonly string[]): boolean {
  if ((RANK[role] ?? 0) < (RANK[grant.role] ?? 0)) return true;
  if (role === "admin") return false;
  return (grant.extras ?? []).some((x) => !extras.includes(x));
}

interface FormProps extends Omit<GrantDialogBase, "open"> {
  mode: "subject" | "env";
  subject?: GrantSubject;
  environment?: GrantEnvironment;
  onPendingChange: (pending: boolean) => void;
}

const NO_GRANTS: readonly Grant[] = [];

function GrantForm({
  mode,
  subject: given,
  environment,
  onOpenChange,
  grant,
  grants = NO_GRANTS,
  onSaved,
  onPendingChange,
}: FormProps) {
  const t = useT();
  const instanceAdmin = useInstanceAdmin();
  const envMode = mode === "env" && environment !== undefined;
  const editing = Boolean(grant);
  const envs = useQuery({ ...environmentsQuery, enabled: !editing && !envMode });
  const candidates = useQuery({
    ...envAccessCandidatesQuery(environment?.id ?? ""),
    enabled: envMode && !grant,
  });
  // The subject: known on a user's or a team's page, chosen in the Who list on
  // an environment's Access tab.
  const [who, setWho] = useState<Who | null>(null);
  const subject: GrantSubject | null = given ?? who;
  const [env, setEnv] = useState(envMode ? (environment?.id ?? "") : grant ? scopeOf(grant) : "");
  const [role, setRole] = useState<RoleId>((grant?.role as RoleId | undefined) ?? "viewer");
  // The extras ticked for an Operator. A Viewer or an Admin shows its own
  // fixed state instead, and this is kept so that going back to Operator
  // restores what was ticked (UI-SPEC "GrantDialog").
  const [extras, setExtras] = useState<string[]>(grant?.extras ?? []);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // A second click can land before the pending state has rendered.
  const inFlight = useRef(false);

  // The grant a save would replace: the one being edited, or the subject's
  // existing grant on the chosen environment.
  const existing = grant ?? (envMode ? undefined : grants.find((g) => scopeOf(g) === env));
  const replacing = !editing && existing !== undefined;
  const envName = envMode
    ? (environment?.name ?? "")
    : env === ALL
      ? t("grants.scope.all")
      : (grant?.envName ?? envs.data?.items?.find((e) => e.id === env)?.name ?? "");
  const sorted = [...(envs.data?.items ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  const loading = !editing && !envMode && envs.isPending;
  // On the Access tab the Who list loads with the dialog; nothing can be saved
  // before someone is chosen in it.
  const whoLoading = envMode && !editing && candidates.isPending;
  const noOneLeft =
    envMode &&
    !editing &&
    candidates.data !== undefined &&
    (candidates.data.users ?? []).length === 0 &&
    (candidates.data.teams ?? []).length === 0;
  // What would be saved for the role: only an Operator carries extras.
  const sentExtras = role === "operator" ? extras : [];
  // Lowering a grant shows what it takes away first, and Save waits for that to
  // load: the preview fails closed. The grant that counts is the one a save
  // replaces, whether it is being edited or "Add access" landed on an
  // environment the subject already holds.
  const target = grant ?? existing;
  const lowering = target ? takesAwayFrom(target, role, sentExtras) : false;
  // Ready is remembered per question, so a preview that loaded for another
  // grant, role or set of extras never lets this one through.
  const previewKey = target ? `${target.id}:${role}:${sentExtras.join(",")}` : "";
  const [readyFor, setReadyFor] = useState<string | null>(null);
  const waiting = lowering && readyFor !== previewKey;
  const canSave = !loading && env !== "" && subject !== null && !pending && !waiting;

  async function save() {
    if (!canSave || inFlight.current || subject === null) return;
    inFlight.current = true;
    setPending(true);
    onPendingChange(true);
    setError(null);
    try {
      const expected = existing
        ? { role: existing.role, extras: existing.extras ?? [] }
        : undefined;
      const result =
        envMode && environment
          ? await putEnvAccess(environment.id, {
              subjectType: subject.type,
              subjectId: subject.id,
              role,
              extras: sentExtras,
              expected,
            })
          : await putGrant(subject.type, subject.id, {
              scope: env === ALL ? "all" : "env",
              envId: env === ALL ? undefined : env,
              role,
              extras: sentExtras,
              expected,
            });
      if (result.kind === "ok") {
        toastSuccess(t("grants.dialog.saved"));
        onSaved();
        onOpenChange(false);
        return;
      }
      setError(
        t(
          {
            viewer_extras: "grants.dialog.errors.viewerExtras",
            forbidden: "grants.dialog.errors.forbidden",
            stale_change: "grants.dialog.errors.stale",
            not_found: "grants.dialog.errors.gone",
            error: "grants.dialog.errors.generic",
          }[result.kind],
        ),
      );
      if (result.kind === "stale_change" || result.kind === "not_found") onSaved();
    } finally {
      inFlight.current = false;
      setPending(false);
      onPendingChange(false);
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle className="break-words">
          {editing ? t("grants.dialog.editTitle") : t("grants.dialog.addTitle")}
        </DialogTitle>
        <DialogDescription className="break-words">
          {editing && subject
            ? t("grants.dialog.editBody", { subject: subject.name, environment: envName })
            : envMode
              ? t("envaccess.dialog.addBody", { environment: envName })
              : t("grants.dialog.addBody", { subject: subject?.name ?? "" })}
        </DialogDescription>
      </DialogHeader>
      <form
        noValidate
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
        className="flex min-w-0 flex-col gap-4"
      >
        {error ? (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        ) : null}
        <fieldset disabled={pending} className="flex min-w-0 flex-col gap-4">
          {envMode && environment ? (
            <>
              {editing && subject ? (
                <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-sm">
                  <dt className="text-muted-foreground">{t("grants.dialog.who")}</dt>
                  <dd className="font-medium break-words">{subject.name}</dd>
                  <dt className="text-muted-foreground">{t("grants.dialog.environment")}</dt>
                  <dd className="font-medium break-words">{environment.name}</dd>
                </dl>
              ) : (
                <>
                  <div className="flex flex-col gap-2">
                    <Label htmlFor="grant-who">{t("grants.dialog.who")}</Label>
                    {whoLoading ? (
                      <div aria-busy="true">
                        <span role="status" className="sr-only">
                          {t("envaccess.dialog.whoLoading")}
                        </span>
                        <Skeleton className="h-11 w-full md:h-8" aria-hidden="true" />
                      </div>
                    ) : candidates.isError ? (
                      <p role="alert" className="text-sm text-destructive">
                        {t("envaccess.dialog.whoFailed")}
                      </p>
                    ) : noOneLeft ? (
                      <p className="text-sm text-muted-foreground">
                        {t("envaccess.dialog.everyone")}
                      </p>
                    ) : (
                      <WhoCombobox
                        id="grant-who"
                        candidates={candidates.data ?? { users: null, teams: null }}
                        value={who}
                        onChange={setWho}
                        disabled={pending}
                      />
                    )}
                  </div>
                  <p className="text-sm font-medium break-words">
                    {t("envaccess.dialog.onEnvironment", { environment: environment.name })}
                  </p>
                </>
              )}
              {instanceAdmin ? null : (
                <Alert role="status">
                  <AlertDescription>
                    {t("envaccess.dialog.delegation", { environment: environment.name })}
                  </AlertDescription>
                </Alert>
              )}
            </>
          ) : editing && subject ? (
            <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-1 text-sm">
              <dt className="text-muted-foreground">{t("grants.dialog.who")}</dt>
              <dd className="font-medium break-words">{subject.name}</dd>
              <dt className="text-muted-foreground">{t("grants.dialog.environment")}</dt>
              <dd className="font-medium break-words">{envName}</dd>
            </dl>
          ) : (
            <div className="flex flex-col gap-2">
              <Label htmlFor="grant-environment">{t("grants.dialog.environment")}</Label>
              {loading ? (
                <div aria-busy="true">
                  <span role="status" className="sr-only">
                    {t("grants.dialog.environmentLoading")}
                  </span>
                  <Skeleton className="h-11 w-full md:h-8" aria-hidden="true" />
                </div>
              ) : envs.isError ? (
                <p role="alert" className="text-sm text-destructive">
                  {t("grants.dialog.environmentsFailed")}
                </p>
              ) : (
                <Select value={env} onValueChange={setEnv}>
                  <SelectTrigger id="grant-environment" className={`${TOUCH_HEIGHT} w-full`}>
                    <SelectValue placeholder={t("grants.dialog.environmentPlaceholder")} />
                  </SelectTrigger>
                  {/* The list scrolls inside the viewport however many environments there are. */}
                  <SelectContent className="max-h-72">
                    <SelectItem value={ALL}>{t("grants.scope.all")}</SelectItem>
                    {sorted.map((e) => (
                      <SelectItem key={e.id} value={e.id}>
                        {e.name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {env === ALL ? (
                <p className="text-xs text-muted-foreground">{t("grants.scope.allDescription")}</p>
              ) : null}
            </div>
          )}
          {replacing && existing && subject ? (
            <Alert role="status">
              <AlertDescription>
                {t("grants.dialog.existing", {
                  subject: subject.name,
                  role: t(`grants.role.${existing.role}.label`),
                })}
              </AlertDescription>
            </Alert>
          ) : null}
          <RolePicker value={role} onChange={setRole} disabled={pending} />
          <ExtraPermissions role={role} value={extras} onChange={setExtras} disabled={pending} />
        </fieldset>
        {lowering && target && subject ? (
          <ImpactPreview
            // A different grant, role or set of extras is a different question.
            key={previewKey}
            load={() => {
              const change = { op: "update" as const, role, extras: sentExtras };
              return envMode && environment
                ? previewEnvAccessImpact(environment.id, target.id, change)
                : previewGrantImpact(subject.type, subject.id, target.id, change);
            }}
            onReadyChange={(ready) => setReadyFor(ready ? previewKey : null)}
          />
        ) : null}
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            className={TOUCH_HEIGHT}
            disabled={pending}
            onClick={() => onOpenChange(false)}
          >
            {t("grants.dialog.cancel")}
          </Button>
          <Button type="submit" className={TOUCH_HEIGHT} disabled={!canSave}>
            {pending ? (
              <>
                <Spinner aria-hidden="true" />
                {t("grants.dialog.pending")}
              </>
            ) : editing || replacing ? (
              t("grants.dialog.save")
            ) : (
              t("grants.dialog.add")
            )}
          </Button>
        </DialogFooter>
      </form>
    </>
  );
}

// GrantDialog is "Add access" and "Edit access" (UI-SPEC "GrantDialog"): the
// environment (a Select with "All environments" first, then each environment
// alphabetically), the role, and the extra permissions. Adding for a subject
// that already holds the chosen environment says that saving replaces it, and
// editing shows who and where as read-only text. A refusal or a failure stays
// inside the dialog as an inline alert, never a toast over it. The form starts
// afresh every time the dialog opens, and the dialog cannot be closed while a
// save is in flight.
export function GrantDialog({ open, onOpenChange, mode = "subject", ...rest }: GrantDialogProps) {
  const [pending, setPending] = useState(false);
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (pending) return;
        onOpenChange(next);
      }}
    >
      <DialogContent
        showCloseButton={false}
        className="max-h-[90svh] overflow-y-auto sm:max-w-[560px]"
      >
        {open ? (
          <GrantForm
            {...rest}
            mode={mode}
            onOpenChange={(next) => {
              setPending(false);
              onOpenChange(next);
            }}
            onPendingChange={setPending}
          />
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
