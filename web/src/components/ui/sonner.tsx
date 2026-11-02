// SPDX-License-Identifier: AGPL-3.0-or-later
// SPDX-FileCopyrightText: 2026 The TierPlan Authors
// SPDX-FileCopyrightText: 2023 shadcn. Derived from shadcn/ui (https://github.com/shadcn-ui/ui), used under the MIT licence; see the third-party notices.
import {
  CircleCheckIcon,
  InfoIcon,
  Loader2Icon,
  OctagonXIcon,
  TriangleAlertIcon,
} from "lucide-react";
import { useSyncExternalStore } from "react";
import { Toaster as Sonner, type ToasterProps } from "sonner";

import { useIsMobile } from "@/hooks/use-mobile";

// The app owns the theme: lib/theme.ts toggles the `dark` class on <html>, so
// the toasts follow that class instead of a theme provider.
function subscribeToThemeClass(onChange: () => void) {
  const observer = new MutationObserver(onChange);
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
  return () => observer.disconnect();
}

function useColorScheme(): "light" | "dark" {
  return useSyncExternalStore(
    subscribeToThemeClass,
    () => (document.documentElement.classList.contains("dark") ? "dark" : "light"),
    () => "light",
  );
}

// Toaster is mounted once by the signed-in layout. UI-SPEC "Toasts": bottom
// right on desktop, top centre under 768px. Durations and live-region roles
// are set per toast by lib/toast.ts.
const Toaster = ({ ...props }: ToasterProps) => {
  const theme = useColorScheme();
  const isMobile = useIsMobile();

  return (
    <Sonner
      theme={theme}
      position={isMobile ? "top-center" : "bottom-right"}
      className="toaster group"
      icons={{
        success: <CircleCheckIcon className="size-4" />,
        info: <InfoIcon className="size-4" />,
        warning: <TriangleAlertIcon className="size-4" />,
        error: <OctagonXIcon className="size-4" />,
        loading: <Loader2Icon className="size-4 animate-spin motion-reduce:animate-none" />,
      }}
      style={
        {
          "--normal-bg": "var(--popover)",
          "--normal-text": "var(--popover-foreground)",
          "--normal-border": "var(--border)",
          "--border-radius": "var(--radius)",
        } as React.CSSProperties
      }
      toastOptions={{
        classNames: {
          toast: "cn-toast",
        },
      }}
      {...props}
    />
  );
};

export { Toaster };
