// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery } from "@tanstack/react-query";
import { useRef, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import { type Grant, putGrant, type SubjectType } from "@/api/grants";
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
import { type RoleId, RolePicker } from "@/features/access/role-picker";
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

interface GrantDialogProps {
  subject: GrantSubject;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // The grant being edited, or null to add one.
  grant?: Grant | null;
  // The subject's own grants, so choosing an environment they already hold can
  // say that saving replaces it.
  grants: readonly Grant[];
  // Called once the grant is saved, before the dialog closes.
  onSaved: () => void;
}

// scopeOf is the Select value a grant is on.
function scopeOf(g: Grant): string {
  return g.scope === "all" ? ALL : (g.envId ?? "");
}

interface FormProps extends Omit<GrantDialogProps, "open"> {
  onPendingChange: (pending: boolean) => void;
}

function GrantForm({ subject, onOpenChange, grant, grants, onSaved, onPendingChange }: FormProps) {
  const t = useT();
  const editing = Boolean(grant);
  const envs = useQuery({ ...environmentsQuery, enabled: !editing });
  const [env, setEnv] = useState(grant ? scopeOf(grant) : "");
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
  const existing = grant ?? grants.find((g) => scopeOf(g) === env);
  const replacing = !editing && existing !== undefined;
  const envName =
    env === ALL
      ? t("grants.scope.all")
      : (grant?.envName ?? envs.data?.items?.find((e) => e.id === env)?.name ?? "");
  const sorted = [...(envs.data?.items ?? [])].sort((a, b) => a.name.localeCompare(b.name));
  const loading = !editing && envs.isPending;
  const canSave = !loading && env !== "" && !pending;

  async function save() {
    if (!canSave || inFlight.current) return;
    inFlight.current = true;
    setPending(true);
    onPendingChange(true);
    setError(null);
    try {
      const result = await putGrant(subject.type, subject.id, {
        scope: env === ALL ? "all" : "env",
        envId: env === ALL ? undefined : env,
        role,
        extras: role === "operator" ? extras : [],
        expected: existing ? { role: existing.role, extras: existing.extras ?? [] } : undefined,
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
          {editing
            ? t("grants.dialog.editBody", { subject: subject.name, environment: envName })
            : t("grants.dialog.addBody", { subject: subject.name })}
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
          {editing ? (
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
          {replacing && existing ? (
            <Alert>
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
export function GrantDialog({ open, onOpenChange, ...rest }: GrantDialogProps) {
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
