// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { createFileRoute } from "@tanstack/react-router";

import { AddressSpacePage } from "@/features/address-space/address-space-page";
import { useDocumentTitle } from "@/hooks/use-me";
import { useT } from "@/i18n/use-t";

function AddressSpace() {
  const { envId } = Route.useParams();
  const t = useT();
  useDocumentTitle(t("addressSpace.page.title"));
  return <AddressSpacePage envId={envId} />;
}

export const Route = createFileRoute("/_app/env/$envId/address-space")({
  component: AddressSpace,
});
