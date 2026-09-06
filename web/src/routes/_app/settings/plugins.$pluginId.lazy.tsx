import { createLazyFileRoute } from "@tanstack/react-router";
import { PersonalUnifiedPluginsPage } from "@/features/plugins/UnifiedPluginsPage";

export const Route = createLazyFileRoute("/_app/settings/plugins/$pluginId")({
  component: PersonalUnifiedPluginsPage,
});
