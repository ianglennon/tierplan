// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ListChecks, TriangleAlert } from "lucide-react";
import type { ReactNode } from "react";

import { stacksQuery } from "@/api/stacks";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Item, ItemContent, ItemMedia } from "@/components/ui/item";
import { CommandBlock } from "@/features/pool-guide/command-block";
import {
  JOIN_TOKEN_COMMAND,
  SWARM_LEAVE_COMMAND,
  type SwarmInputs,
  stackRemoveCommands,
} from "@/features/pool-guide/generate";
import { SwarmCommand, SwarmFields, useSwarmRead } from "@/features/pool-guide/new-swarm-form";
import { useT } from "@/i18n/use-t";

interface StepProps {
  n: number;
  title: string;
  children: ReactNode;
}

// Step is one numbered item of the runbook. The list around it is an ol, so the order is real to
// a screen reader; the number beside it is only for the eye.
function Step({ n, title, children }: StepProps) {
  return (
    <li>
      <Item variant="outline" className="items-start">
        <ItemMedia
          aria-hidden="true"
          className="size-6 rounded-full bg-muted text-xs font-medium tabular-nums"
        >
          {n}
        </ItemMedia>
        <ItemContent className="min-w-0 gap-2">
          <h3 className="text-sm font-medium">{title}</h3>
          {children}
        </ItemContent>
      </Item>
    </li>
  );
}

function Body({ children }: { children: ReactNode }) {
  return <p className="text-sm text-muted-foreground">{children}</p>;
}

interface MigrationRunbookProps {
  envId: string;
  inputs: SwarmInputs;
  onChange: (patch: Partial<SwarmInputs>) => void;
  // True when the environment is a swarm, whose Coaming stacks can be named in the remove step.
  swarm: boolean;
}

// MigrationRunbook is the Rebuild a swarm tab (D-23 part 2): the downtime said plainly and from the
// first moment, the seven steps in order, and the pool fields it shares with New swarm. The stack
// names come from the stacks Coaming already lists; a name that is not one plain word gets no
// command, only the sentence that sends the person to the Stacks page.
export function MigrationRunbook({ envId, inputs, onChange, swarm }: MigrationRunbookProps) {
  const t = useT();
  const read = useSwarmRead(inputs);
  const stacks = useQuery({ ...stacksQuery(envId, { kind: "managed" }), enabled: swarm });
  const names = (stacks.data?.items ?? []).filter((s) => s.managed).map((s) => s.name);
  const removes = stackRemoveCommands(names);
  const skipped = names.length - removes.length;
  return (
    <div className="flex flex-col gap-4">
      <p className="flex items-center gap-2 text-sm text-muted-foreground">
        <ListChecks aria-hidden="true" className="size-4 shrink-0" />
        {t("poolGuide.rebuild.intro")}
      </p>
      {/* role="status" and present from load: the warning is read before anything else is. */}
      <Alert variant="warning" role="status">
        <TriangleAlert aria-hidden="true" />
        <AlertDescription className="text-foreground">
          {t("poolGuide.rebuild.downtime")}
        </AlertDescription>
      </Alert>
      <div className="grid gap-8 lg:grid-cols-[2fr_3fr]">
        <div>
          <SwarmFields inputs={inputs} onChange={onChange} read={read} />
        </div>
        <ol aria-label={t("poolGuide.rebuild.stepsLabel")} className="flex min-w-0 flex-col gap-4">
          <Step n={1} title={t("poolGuide.rebuild.steps.backup.title")}>
            <Body>{t("poolGuide.rebuild.steps.backup.body")}</Body>
          </Step>
          <Step n={2} title={t("poolGuide.rebuild.steps.remove.title")}>
            <Body>
              {t(
                removes.length > 0
                  ? "poolGuide.rebuild.steps.remove.body"
                  : "poolGuide.rebuild.steps.remove.bodyPlain",
              )}
            </Body>
            {removes.length > 0 ? (
              <CommandBlock
                caption={t("poolGuide.rebuild.captions.manager")}
                command={removes.join("\n")}
              />
            ) : null}
            {skipped > 0 ? (
              <Body>{t("poolGuide.rebuild.steps.remove.skipped", { count: skipped })}</Body>
            ) : null}
          </Step>
          <Step n={3} title={t("poolGuide.rebuild.steps.leave.title")}>
            <Body>{t("poolGuide.rebuild.steps.leave.body")}</Body>
            <CommandBlock
              caption={t("poolGuide.rebuild.captions.everyNode")}
              command={SWARM_LEAVE_COMMAND}
            />
          </Step>
          <Step n={4} title={t("poolGuide.rebuild.steps.create.title")}>
            {read.kind === "invalid" ? (
              <Body>{t("poolGuide.rebuild.steps.create.waiting")}</Body>
            ) : (
              <SwarmCommand read={read} caption={t("poolGuide.rebuild.captions.firstManager")} />
            )}
          </Step>
          <Step n={5} title={t("poolGuide.rebuild.steps.rejoin.title")}>
            <Body>{t("poolGuide.rebuild.steps.rejoin.body")}</Body>
            <CommandBlock
              caption={t("poolGuide.rebuild.captions.manager")}
              command={JOIN_TOKEN_COMMAND}
            />
          </Step>
          <Step n={6} title={t("poolGuide.rebuild.steps.adopt.title")}>
            <Body>{t("poolGuide.rebuild.steps.adopt.body")}</Body>
            <Link
              to="/settings/environments/$envId"
              params={{ envId }}
              className="w-fit rounded-sm text-sm text-accent-ink underline underline-offset-4 max-md:py-3"
            >
              {t("poolGuide.rebuild.steps.adopt.link")}
            </Link>
          </Step>
          <Step n={7} title={t("poolGuide.rebuild.steps.redeploy.title")}>
            <Body>{t("poolGuide.rebuild.steps.redeploy.body")}</Body>
          </Step>
        </ol>
      </div>
    </div>
  );
}
