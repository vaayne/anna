import { describe, expect, it } from "vitest";
import { registryPluginNamespace } from "./useMcpMarketInstall";

describe("registryPluginNamespace", () => {
  it("normalizes registry path punctuation without changing collision behavior", () => {
    expect(registryPluginNamespace("com.stella/registry-add")).toBe("com-stella-registry-add");
    expect(registryPluginNamespace("vendor..server/")).toBe("vendor-server");
  });

  it("rejects an id with no namespace-safe content", () => {
    expect(() => registryPluginNamespace("///")).toThrow(
      "registry server id cannot produce a valid plugin namespace",
    );
  });
});
