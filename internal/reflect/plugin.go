package reflect

import pkgplugins "github.com/CherryHQ/stella/pkg/plugins"

// PluginID is the stable identity used by the unified plugin catalog and the
// persisted snapshot resolver for Reflect's background review service.
const PluginID = "system/reflect"

func init() {
	pkgplugins.Register(PluginID, pkgplugins.PluginFunc(func(host pkgplugins.Host) {
		host.SetInfo(pkgplugins.PluginInfo{
			ID:           PluginID,
			Kind:         "system",
			Name:         "reflect",
			DisplayName:  "Reflect",
			Description:  "Background conversation review and skill usage curation.",
			AdminVisible: true,
		})
	}))
}
