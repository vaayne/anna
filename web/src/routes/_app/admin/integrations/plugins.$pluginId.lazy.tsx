import { createLazyFileRoute } from "@tanstack/react-router";
import { UnifiedPluginsPage } from "@/features/plugins/UnifiedPluginsPage";

export const Route = createLazyFileRoute("/_app/admin/integrations/plugins/$pluginId")({
  component: UnifiedPluginsPage,
});
