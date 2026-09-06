import { UnifiedPluginsPage } from "@/features/plugins/UnifiedPluginsPage";
import type { ScopeBand } from "@/lib/scope-band";

/**
 * Compatibility route components for bookmarks and the existing settings
 * route. The rendered surface is the common plugin inventory, which owns
 * registry browsing, safe config editing, and nested OAuth actions.
 */
export function MCPServersPage({ scopeBand }: { scopeBand: ScopeBand }) {
  return <UnifiedPluginsPage scopeBand={scopeBand} />;
}

export function PersonalMCPPage() {
  return <MCPServersPage scopeBand="personal" />;
}

export function GlobalMCPPage() {
  return <MCPServersPage scopeBand="system" />;
}
