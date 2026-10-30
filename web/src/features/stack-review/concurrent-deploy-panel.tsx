// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { GitCompareArrows } from "lucide-react";
import { useId, useState } from "react";
import { Trans } from "react-i18next";

import type { StackConcurrent, StackFileDiff } from "@/api/stacks";
import { RelativeTime } from "@/components/relative-time";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { YamlDiff } from "@/features/stack-review/yaml-diff";
import { useT } from "@/i18n/use-t";

interface ConcurrentDeployPanelProps {
  // What was deployed after the draft was started, as the review answered it.
  concurrent: StackConcurrent;
  acknowledged: boolean;
  onAcknowledge: (acknowledged: boolean) => void;
}

// FileChanges is the files one version changed, each under a group row that names it, and a diff
// of its masked text. A secret file or variable that differs says so and shows nothing of it.
function FileChanges({ files }: { files: StackFileDiff[] | null }) {
  const t = useT();
  const list = files ?? [];
  if (list.length === 0) {
    return (
      <p className="text-sm text-muted-foreground">{t("stackYamlDiff.concurrent.dialog.none")}</p>
    );
  }
  return (
    <div className="flex flex-col gap-4">
      {list.map((file) => {
        const name =
          file.kind === "variables" ? t("stackYamlDiff.concurrent.dialog.variables") : file.path;
        const hasHunks = (file.hunks ?? []).length > 0;
        return (
          <section key={`${file.kind}:${file.path}`} className="flex flex-col gap-2">
            <h4 className="rounded-md bg-muted px-2 py-1.5 font-mono text-xs font-semibold break-all">
              {name}
            </h4>
            {hasHunks ? (
              <YamlDiff
                hunks={file.hunks}
                lines={file.lines}
                caption={t("stackYamlDiff.concurrent.dialog.diff", { path: name })}
              />
            ) : null}
            {file.hidden ? (
              <p className="text-sm text-muted-foreground">
                {t("stackYamlDiff.concurrent.dialog.hidden")}
              </p>
            ) : null}
          </section>
        );
      })}
    </div>
  );
}

// ConcurrentDeployPanel is the warning that someone deployed after the person started editing
// (STK-05, D-09), the dialog that shows what changed, and the checkbox that gates Deploy stack.
// The server refuses the deploy without the acknowledgement as well; this is how the person gives
// it. Every name and line is text (T-07-71).
export function ConcurrentDeployPanel({
  concurrent,
  acknowledged,
  onAcknowledge,
}: ConcurrentDeployPanelProps) {
  const t = useT();
  const [open, setOpen] = useState(false);
  const boxId = useId();
  return (
    <>
      <Alert variant="warning">
        <GitCompareArrows aria-hidden="true" />
        <AlertTitle>{t("stackYamlDiff.concurrent.title")}</AlertTitle>
        <AlertDescription className="flex flex-col gap-3">
          <p>
            <Trans
              i18nKey="stackYamlDiff.concurrent.body"
              ns="stackYamlDiff"
              values={{ user: concurrent.deployedBy }}
              components={{ time: <RelativeTime value={concurrent.deployedAt} /> }}
            />
          </p>
          <div>
            <Button
              type="button"
              variant="outline"
              size="sm"
              className="max-md:h-11"
              onClick={() => setOpen(true)}
            >
              {t("stackYamlDiff.concurrent.see")}
            </Button>
          </div>
          <div className="flex items-start gap-2">
            <Checkbox
              id={boxId}
              checked={acknowledged}
              onCheckedChange={(checked) => onAcknowledge(checked === true)}
              className="mt-0.5"
            />
            <Label htmlFor={boxId} className="text-sm leading-snug font-normal text-foreground">
              {t("stackYamlDiff.concurrent.ack")}
            </Label>
          </div>
        </AlertDescription>
      </Alert>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-200">
          <DialogHeader>
            <DialogTitle>{t("stackYamlDiff.concurrent.dialog.title")}</DialogTitle>
            <DialogDescription>{t("stackYamlDiff.concurrent.dialog.body")}</DialogDescription>
          </DialogHeader>
          <Tabs defaultValue="deployed" className="gap-4">
            <TabsList variant="line" className="h-9">
              <TabsTrigger value="deployed" className="max-md:h-11">
                {t("stackYamlDiff.concurrent.dialog.deployed")}
              </TabsTrigger>
              <TabsTrigger value="mine" className="max-md:h-11">
                {t("stackYamlDiff.concurrent.dialog.mine")}
              </TabsTrigger>
            </TabsList>
            <TabsContent value="deployed">
              <FileChanges files={concurrent.baseToDeployed} />
            </TabsContent>
            <TabsContent value="mine">
              <FileChanges files={concurrent.baseToDraft} />
            </TabsContent>
          </Tabs>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline" className="max-md:h-11">
                {t("stackYamlDiff.concurrent.dialog.close")}
              </Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
