package upgrade

import "github.com/jenkins-x/jx/pkg/plugins"

// This file holds the two lookup tables the upgrade command is driven from: which declared plugins a
// flag combination covers, and which hooks run after a plugin is installed. Both used to be implicit in
// the body of Run, which made it hard to see what a flag combination would actually touch.

const (
	// scopeDefault the flag combination with neither --mandatory nor --boot
	scopeDefault = "default"
	// scopeMandatory --mandatory
	scopeMandatory = "mandatory"
	// scopeBoot --boot
	scopeBoot = "boot"

	// hookModeBoot a hook that only applies in --boot mode
	hookModeBoot = "boot"
	// hookModeNonBoot a hook that only applies outside --boot mode
	hookModeNonBoot = "non-boot"

	// hookPathPlaceholder is replaced by the --path flag value in a hook's arguments
	hookPathPlaceholder = "{path}"
)

// pluginScope the set of declared plugins a flag combination covers, and whether plugins found only in
// the plugin directory take part
type pluginScope struct {
	// Plugins the declared plugin names in scope
	Plugins map[string]bool
	// IncludeExtras whether plugins found in the plugin directory but not declared take part
	IncludeExtras bool
}

// pluginScopeTable the plugins each flag combination covers.
//
// Note that --mandatory does not narrow the declared plugins at all: it only skips the extra plugins
// found in the plugin directory. Its help text used to claim it "ignores optional plugins", which the
// implementation never did; the help text now describes the real behaviour.
var pluginScopeTable = map[string]pluginScope{
	scopeDefault: {
		Plugins:       declaredPluginSet(),
		IncludeExtras: true,
	},
	scopeMandatory: {
		Plugins:       declaredPluginSet(),
		IncludeExtras: false,
	},
	scopeBoot: {
		Plugins:       bootPluginSet(),
		IncludeExtras: false,
	},
}

// bootPluginNames the five plugins needed for boot. The plan order still comes from plugins.Plugins, so
// this list only needs to name them, it does not set their order.
var bootPluginNames = []string{
	"gitops",
	"health",
	"secret",
	"verify",
	"changelog",
}

// pluginHook a command run after a plugin has been ensured installed
type pluginHook struct {
	// Name the plugin whose binary runs the hook
	Name string
	// Args the hook arguments; hookPathPlaceholder is replaced by the --path flag value
	Args []string
	// Mode the mode the hook applies in
	Mode string
}

// pluginHooks the explicit hook table. The hook used to be hardcoded in the loop over plugins.Plugins;
// a TODO in the source suggested driving it from plugin metadata instead. That is intentionally not done
// here - this table keeps the existing plugin names and arguments, it only makes them visible.
var pluginHooks = []pluginHook{
	{
		Name: "gitops",
		Args: []string{"plugin", "upgrade", "--path", hookPathPlaceholder},
		Mode: hookModeBoot,
	},
	{
		Name: "secret",
		Args: []string{"plugins", "upgrade"},
		Mode: hookModeNonBoot,
	},
}

// declaredPluginSet the names of every declared plugin
func declaredPluginSet() map[string]bool {
	set := make(map[string]bool, len(plugins.Plugins))
	for i := range plugins.Plugins {
		set[plugins.Plugins[i].Name] = true
	}
	return set
}

// bootPluginSet the names of the plugins needed for boot
func bootPluginSet() map[string]bool {
	set := make(map[string]bool, len(bootPluginNames))
	for _, name := range bootPluginNames {
		set[name] = true
	}
	return set
}

// scope the table entry for the options
func (o *PluginOptions) scope() pluginScope {
	return pluginScopeTable[o.scopeName()]
}

// scopeName the table key for the options
func (o *PluginOptions) scopeName() string {
	switch {
	case o.Boot:
		return scopeBoot
	case o.OnlyMandatory:
		return scopeMandatory
	default:
		return scopeDefault
	}
}

// hookFor the hook that applies to the given plugin and mode, or nil if none does
func hookFor(pluginName string, boot bool) *pluginHook {
	mode := hookModeNonBoot
	if boot {
		mode = hookModeBoot
	}
	for i := range pluginHooks {
		hook := &pluginHooks[i]
		if hook.Name == pluginName && hook.Mode == mode {
			return hook
		}
	}
	return nil
}

// hookArgs the hook arguments with the --path value filled in
func (h *pluginHook) hookArgs(path string) []string {
	args := make([]string, len(h.Args))
	for i, arg := range h.Args {
		if arg == hookPathPlaceholder {
			args[i] = path
			continue
		}
		args[i] = arg
	}
	return args
}
