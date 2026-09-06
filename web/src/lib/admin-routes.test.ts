import { describe, expect, it } from "vitest";
import {
  adminCompatibilityHref,
  libraryCompatibilityHref,
  personalCompatibilityHref,
} from "@/lib/admin-routes";

describe("adminCompatibilityHref", () => {
  it.each([
    ["/settings/providers", "", "/admin/ai/providers"],
    ["/settings/providers/openai", "?tab=models", "/admin/ai/providers/openai?tab=models"],
    ["/settings/embedding", "", "/admin/ai/models"],
    ["/settings/vision", "?model=current", "/admin/ai/models?model=current"],
    ["/settings/provisioning", "", "/admin/access/provisioning"],
    ["/settings/users", "?state=active", "/admin/users?state=active"],
    ["/settings/users/user-1", "", "/admin/users/user-1"],
    [
      "/settings/plugins/telegram",
      "?tab=config",
      "/admin/integrations/plugins/telegram?tab=config",
    ],
    ["/settings/about", "", "/admin/overview"],
  ])("maps %s%s", (pathname, search, expected) => {
    expect(adminCompatibilityHref(pathname, search)).toBe(expected);
  });

  it("does not redirect personal routes", () => {
    expect(adminCompatibilityHref("/settings/account")).toBeNull();
    expect(adminCompatibilityHref("/settings/credentials")).toBeNull();
    expect(adminCompatibilityHref("/settings/plugins")).toBeNull();
  });
});

describe("personalCompatibilityHref", () => {
  it("leaves the active unified Plugins root in place", () => {
    expect(personalCompatibilityHref("/settings/plugins", "?from=bookmark")).toBeNull();
    expect(personalCompatibilityHref("/settings/plugins/tool-id")).toBeNull();
  });
});

describe("libraryCompatibilityHref", () => {
  it("moves only deployment-owned Library bookmarks", () => {
    expect(
      libraryCompatibilityHref("/settings/library", "?scope=system_agent&agent=a&q=runbook"),
    ).toBe("/admin/resources/library?scope=system_agent&agent=a&q=runbook");
    expect(libraryCompatibilityHref("/settings/library", "?q=mine")).toBeNull();
  });
});
