// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CirclePause } from "lucide-react";
import { useId, useMemo, useState } from "react";

import { environmentsQuery } from "@/api/environments";
import type { OwnedItem } from "@/api/owned";
import { type StackGit, stackGitKey, useSetStackGitAutoDeploy } from "@/api/stack-git";
import { NeedsTooltip } from "@/components/needs-tooltip";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { TransferOwnershipDialog } from "@/features/access/transfer-ownership-dialog";
import { AutoDeployConfirmDialog } from "@/features/stack-git/auto-deploy-confirm-dialog";
import { useNeeds } from "@/hooks/use-needs";
import { useT } from "@/i18n/use-t";
import { toastError, toastSuccess } from "@/lib/toast";

// The kind the automatic deploys of a stack are registered under with the access service.
const AUTO_DEPLOY_KIND = "git_autodeploy";

interface AutoDeployCardProps {
  envId: string;
  stackId: string;
  stackName: string;
  git: StackGit;
}

// AutoDeployCard is the Deploying card of the Git tab (UI-SPEC "Git tab", D-18, GIT-07): whether a new commit that changes
// the stack's folder deploys by itself or waits for a person. The switch reads the saved state and nothing else: turning
// it on opens the confirm and is applied only when the confirm succeeds, and a cancelled confirm leaves it off. Turning it
// off needs no confirm. While it is on the card names the person it runs for, with Transfer… for an instance Admin (the
// access service's transfer needs the users.manage permission); while it is paused it says so, with Resume…, which turns it
// on again for the person pressing it after the same confirm. A stack that follows a fixed commit has nothing to deploy,
// so the switch is not drawn at all.
export function AutoDeployCard({ envId, stackId, stackName, git }: AutoDeployCardProps) {
  const t = useT();
  const queryClient = useQueryClient();
  const headingId = useId();
  const switchId = useId();
  const helperId = useId();
  const needs = useNeeds("stacks.deploy", envId);
  const canTransfer = useNeeds("users.manage").allowed;
  const environments = useQuery(environmentsQuery);
  const set = useSetStackGitAutoDeploy(envId, stackId);
  const [confirming, setConfirming] = useState(false);
  const [transferring, setTransferring] = useState(false);

  const envName = environments.data?.items?.find((e) => e.id === envId)?.name ?? "";
  const canChange = needs.allowed;
  // Permissions not known yet: the switch is off, with no reason to give.
  const unknown = !needs.allowed && !needs.reason;
  const reason = needs.reason
    ? envName
      ? t("stackGit.autoDeploy.needsOnEnvironment", { need: needs.reason, name: envName })
      : needs.reason
    : undefined;
  const fixed = git.refKind === "commit";
  const folder = git.folder || t("stackGit.autoDeploy.folderRoot");
  const owner = git.autoDeployOwner;
  const ownerName = owner?.name || t("stackGit.autoDeploy.ownerFallback");

  // What the Transfer… dialog names and hands over: the access service knows the automation by its kind and the stack's ID.
  const transferItem: OwnedItem | null = useMemo(
    () =>
      transferring
        ? {
            kind: AUTO_DEPLOY_KIND,
            label: "Git auto-deploy",
            resourceId: stackId,
            name: stackName,
            envId,
            envName,
            state: git.autoDeployPaused ? "paused_lacks_permission" : "active",
            needsEnvAdmin: false,
          }
        : null,
    [transferring, stackId, stackName, envId, envName, git.autoDeployPaused],
  );

  function change(next: boolean) {
    if (next) {
      // Not applied until the confirm says so: the switch keeps reading the saved state.
      setConfirming(true);
      return;
    }
    if (set.isPending) return;
    set.mutate(false, {
      onSuccess: () => toastSuccess(t("stackGit.autoDeploy.turnedOff", { name: stackName })),
      onError: () => toastError(t("stackGit.autoDeploy.failed")),
    });
  }

  const resume = (
    <Button
      type="button"
      variant="outline"
      size="sm"
      className="max-md:h-11"
      disabled={unknown}
      onClick={() => setConfirming(true)}
    >
      {t("stackGit.autoDeploy.resume")}
    </Button>
  );
  const toggle = (
    <Switch
      id={switchId}
      checked={git.autoDeploy}
      onCheckedChange={change}
      disabled={unknown || (canChange && set.isPending)}
      aria-describedby={helperId}
    />
  );

  return (
    <section aria-labelledby={headingId}>
      <Card className="gap-4 [--card-spacing:--spacing(4)] md:[--card-spacing:--spacing(6)]">
        <CardHeader>
          <CardTitle>
            <h3 id={headingId} className="text-base font-semibold">
              {t("stackGit.autoDeploy.title")}
            </h3>
          </CardTitle>
          <CardDescription>{t("stackGit.autoDeploy.body")}</CardDescription>
        </CardHeader>
        <CardContent className="flex min-w-0 flex-col gap-4">
          {fixed ? (
            <p className="text-sm text-muted-foreground">{t("stackGit.autoDeploy.fixed")}</p>
          ) : (
            <>
              <div className="flex items-center gap-2 max-md:min-h-11">
                {canChange || unknown ? (
                  toggle
                ) : (
                  <NeedsTooltip reason={reason}>{toggle}</NeedsTooltip>
                )}
                <Label htmlFor={switchId}>{t("stackGit.autoDeploy.switch")}</Label>
              </div>
              <p id={helperId} className="text-sm text-muted-foreground break-words">
                {git.autoDeploy
                  ? t("stackGit.autoDeploy.helperOn", { folder, user: ownerName })
                  : t("stackGit.autoDeploy.helperOff", { folder, name: stackName })}
              </p>
              {git.autoDeploy ? (
                <div className="flex flex-wrap items-center gap-x-6 gap-y-2 text-sm">
                  <span className="text-muted-foreground">{t("stackGit.autoDeploy.runsAs")}</span>
                  <span className="font-medium break-all">{ownerName}</span>
                  {canTransfer ? (
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      className="max-md:h-11"
                      aria-label={t("stackGit.autoDeploy.transferLabel", { name: stackName })}
                      onClick={() => setTransferring(true)}
                    >
                      {t("stackGit.autoDeploy.transfer")}
                    </Button>
                  ) : null}
                  {git.autoDeployPaused ? (
                    <>
                      <Badge variant="outline">
                        <CirclePause aria-hidden="true" className="text-status-warning-text" />
                        {t("stackGit.autoDeploy.paused")}
                      </Badge>
                      {canChange || unknown ? (
                        resume
                      ) : (
                        <NeedsTooltip reason={reason}>{resume}</NeedsTooltip>
                      )}
                    </>
                  ) : null}
                </div>
              ) : null}
            </>
          )}
        </CardContent>
      </Card>
      <AutoDeployConfirmDialog
        envId={envId}
        stackId={stackId}
        stackName={stackName}
        gitRef={git.ref}
        folder={git.folder}
        environmentName={envName}
        open={confirming}
        onOpenChange={setConfirming}
      />
      <TransferOwnershipDialog
        item={transferItem}
        onClose={() => setTransferring(false)}
        onDone={() => {
          setTransferring(false);
          void queryClient.invalidateQueries({ queryKey: stackGitKey(envId, stackId) });
        }}
        onStale={() =>
          void queryClient.invalidateQueries({ queryKey: stackGitKey(envId, stackId) })
        }
      />
    </section>
  );
}
