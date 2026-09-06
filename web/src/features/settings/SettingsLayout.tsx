import { useQuery } from "@tanstack/react-query";
import {
  Bot,
  Blocks,
  CircleUserRound,
  Info,
  KeyRound,
  Library,
  MessageSquare,
  Puzzle,
  Webhook,
} from "lucide-react";
import { meQueryOptions } from "@/lib/queries/me";
import type { SettingsNavGroup } from "@/features/settings/SettingsSurfaceLayout";
import { SettingsSurfaceLayout } from "@/features/settings/SettingsSurfaceLayout";

export const personalSettingsNav: SettingsNavGroup[] = [
  {
    label: "settings.section.resources",
    items: [
      { label: "settings.nav.agents", href: "/settings/agents", icon: Bot },
      { label: "settings.nav.channels", href: "/settings/channels", icon: MessageSquare },
      { label: "settings.nav.webhooks", href: "/settings/webhooks", icon: Webhook },
      { label: "settings.nav.connections", href: "/settings/credentials", icon: KeyRound },
      { label: "settings.nav.library", href: "/settings/library", icon: Library },
      { label: "settings.nav.skills", href: "/settings/skills", icon: Puzzle },
      { label: "settings.nav.plugins", href: "/settings/plugins", icon: Blocks },
    ],
  },
  {
    label: "settings.section.account",
    items: [{ label: "settings.nav.account", href: "/settings/account", icon: CircleUserRound }],
  },
];

const aboutNav: SettingsNavGroup = {
  label: "settings.section.about",
  items: [{ label: "settings.nav.about", href: "/settings/about", icon: Info }],
};

export function personalSettingsGroups(isAdmin: boolean): SettingsNavGroup[] {
  return isAdmin ? personalSettingsNav : [...personalSettingsNav, aboutNav];
}

export function SettingsLayout() {
  const { data: me } = useQuery(meQueryOptions);
  const groups = personalSettingsGroups(!!me?.is_admin);

  return <SettingsSurfaceLayout title="nav.personalSettings" groups={groups} />;
}
