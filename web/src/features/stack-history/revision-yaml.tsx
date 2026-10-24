// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Bayplan Authors

import type { StackRevisionDetail } from "@/api/stack-history";
import { YamlDiff } from "@/features/stack-review/yaml-diff";
import { useT } from "@/i18n/use-t";

interface RevisionYamlProps {
  detail: StackRevisionDetail;
}

// RevisionYaml is the rendered YAML of a revision against the deployed revision's (STK-08, D-05). The
// server computed the difference from the two stored renderings, so both are masked already. When there is
// none it says why: this is the deployed revision, or another revision has the same content, or there is
// nothing to compare with.
export function RevisionYaml({ detail }: RevisionYamlProps) {
  const t = useT();
  const seq = detail.item.seq;
  const hunks = detail.yaml.hunks ?? [];
  if (hunks.length === 0) {
    const which =
      detail.item.deployed || detail.deployedSeq === seq
        ? "deployed"
        : detail.rendered === ""
          ? "noRendering"
          : detail.deployedSeq === 0
            ? "noDeployed"
            : "same";
    return (
      <div className="flex flex-col gap-1 rounded-lg border p-4">
        <p className="font-medium">{t(`stackRevision.yaml.${which}.title`)}</p>
        <p className="text-sm text-muted-foreground">
          {t(`stackRevision.yaml.${which}.body`, { seq })}
        </p>
      </div>
    );
  }
  const caption = t("stackRevision.yaml.caption", { seq, deployed: detail.deployedSeq });
  return (
    <>
      <p className="text-sm text-muted-foreground">{caption}</p>
      {/* A long diff scrolls inside the card, with the unchanged runs between hunks collapsed. */}
      <div className="max-h-[70vh] overflow-y-auto rounded-md">
        <YamlDiff hunks={hunks} lines={detail.yaml.lines} caption={caption} />
      </div>
    </>
  );
}
