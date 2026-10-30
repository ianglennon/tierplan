// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The Coaming Authors

import { useQuery } from "@tanstack/react-query";

import { unmanagedStackQuery, unmanagedStackResourcesQuery } from "@/api/unmanaged-stacks";
import { StackResourcesView } from "@/features/stacks/stack-resources";

interface UnmanagedResourcesProps {
  envId: string;
  name: string;
}

// UnmanagedResources is the Resources tab of a stack Coaming did not make (UI-SPEC "Unmanaged stack page"): the
// cards of a managed stack's tab over what the stack's services use. Its ownership badges preview adopting: a
// network or a volume that carries the stack's label reads Stack, any other External, and a secret or a config
// always External. A card with nothing in it is left out. Every name is Docker's own text, drawn as text.
export function UnmanagedResources({ envId, name }: UnmanagedResourcesProps) {
  const stack = useQuery(unmanagedStackQuery(envId, name)).data;
  const target = stack?.target ?? "swarm";
  const query = useQuery(unmanagedStackResourcesQuery(envId, name, { target }));
  return (
    <StackResourcesView
      envId={envId}
      data={query.data}
      isError={query.isError}
      isFetching={query.isFetching}
      onRetry={() => void query.refetch()}
      swarm={target === "swarm"}
      idle={false}
      preview
    />
  );
}
