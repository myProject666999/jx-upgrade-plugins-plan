package upgrade

import (
	"fmt"
	"io"
	"os"

	jxCore "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/helper"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cobras/templates"
	"github.com/jenkins-x/jx-helpers/v3/pkg/extensions"
	"github.com/jenkins-x/jx-helpers/v3/pkg/homedir"
	"github.com/jenkins-x/jx-helpers/v3/pkg/termcolor"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/jenkins-x/jx/pkg/plugins"

	"github.com/spf13/cobra"
)

var (
	cmdPluginsLong = templates.LongDesc(`
		Upgrades all of the plugins in your local JayeX CLI
	`)

	cmdPluginsExample = templates.Examples(`
		# upgrades your plugin binaries
		jx upgrade plugins

		# show what an upgrade would do without installing anything
		jx upgrade plugins --dry-run
	`)
)

// PluginOptions the options for upgrading plugins
type PluginOptions struct {
	CommandRunner cmdrunner.CommandRunner
	OnlyMandatory bool
	Boot          bool
	DryRun        bool
	Path          string
	// PluginBinDir overrides the plugin directory; tests use it to point at t.TempDir()
	PluginBinDir string
	// Out where the plan is written; defaults to stdout
	Out io.Writer
	// InstallPlugin ensures a declared plugin is installed; defaults to extensions.EnsurePluginInstalled
	InstallPlugin func(plugin jxCore.Plugin, dir string) (string, error)
	// InstallExtraPlugin looks up and installs the latest release of an extra plugin. It is only called
	// for an extra plugin whose local files prove an upstream source, and never during a dry run.
	// Defaults to plugins.InstallStandardPlugin.
	InstallExtraPlugin func(dir, name string) (string, error)
}

// NewCmdUpgradePlugins creates a command object for upgrading plugins
func NewCmdUpgradePlugins() (*cobra.Command, *PluginOptions) {
	o := &PluginOptions{}

	cmd := &cobra.Command{
		Use:     "plugins",
		Short:   "Upgrades all of the plugins in your local JayeX CLI",
		Long:    cmdPluginsLong,
		Example: cmdPluginsExample,
		Run: func(_ *cobra.Command, _ []string) {
			err := o.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().BoolVarP(&o.OnlyMandatory, "mandatory", "m", false, "only upgrade plugins in the top level jx CLI and ignore any extra plugins found in the plugin directory")
	cmd.Flags().BoolVarP(&o.Boot, "boot", "", false, "only install plugins required for boot")
	cmd.Flags().BoolVarP(&o.DryRun, "dry-run", "", false, "show the upgrade plan without installing plugins, running hooks or creating the plugin directory")
	cmd.Flags().StringVarP(&o.Path, "path", "", "/usr/bin", "creates a symlink to the binary plugins in this bin path dir")

	return cmd, o
}

// Run implements the command
func (o *PluginOptions) Run() error {
	if o.CommandRunner == nil {
		o.CommandRunner = cmdrunner.DefaultCommandRunner
	}
	if o.InstallPlugin == nil {
		o.InstallPlugin = extensions.EnsurePluginInstalled
	}
	if o.InstallExtraPlugin == nil {
		o.InstallExtraPlugin = plugins.InstallStandardPlugin
	}
	if o.Out == nil {
		o.Out = os.Stdout
	}

	pluginBinDir, err := o.pluginBinDir()
	if err != nil {
		return err
	}

	plan, err := o.createPlan(pluginBinDir)
	if err != nil {
		return err
	}
	plan.Render(o.Out)

	if o.DryRun {
		log.Logger().Infof("dry run: not installing plugins, not running hooks and not creating %s", pluginBinDir)
		return nil
	}

	return o.execute(pluginBinDir, plan)
}

// pluginBinDir resolves the plugin directory. A dry run must not create anything, and
// homedir.DefaultPluginBinDir creates the directory as a side effect, so a dry run resolves the same
// path without creating it.
func (o *PluginOptions) pluginBinDir() (string, error) {
	if o.PluginBinDir != "" {
		return o.PluginBinDir, nil
	}
	if o.DryRun {
		return readOnlyPluginBinDir(), nil
	}
	pluginBinDir, err := homedir.DefaultPluginBinDir()
	if err != nil {
		return "", fmt.Errorf("failed to find plugin bin directory: %w", err)
	}
	return pluginBinDir, nil
}

// execute carries out the plan. A failing plugin is recorded and the rest of the plan still runs, so
// that one broken plugin no longer stops all the other plugins from being upgraded. The returned error
// lists every failure and so makes the command exit non-zero.
func (o *PluginOptions) execute(pluginBinDir string, plan *pluginPlan) error {
	failures := 0
	for _, entry := range plan.Entries {
		if err := o.executeEntry(pluginBinDir, entry); err != nil {
			failures++
			log.Logger().Errorf("failed to upgrade plugin %s: %+v", termcolor.ColorInfo(entry.Name), err)
			fmt.Fprintf(o.Out, failedRowFormat, entry.Name, err)
		}
	}

	for _, entry := range plan.Extras {
		switch entry.Action {
		case actionSkipUnknownSource:
			// the plugin directory holds no evidence of where this plugin came from, so upgrading it
			// would mean guessing an upstream repository. It is reported and left alone.
			log.Logger().Warnf("skipping plugin %s: no local evidence of its upstream source, so it is not upgraded; remove it from %s to silence this warning",
				termcolor.ColorInfo(entry.Name), pluginBinDir)
		case actionQueryLatest:
			// this only runs when the plugin directory proves an upstream source, so the lookup is not
			// a guess. Failures only warn: an extra plugin never changes the exit status.
			log.Logger().Infof("upgrading plugin %s to its latest release", termcolor.ColorInfo(entry.Name))
			if _, err := o.InstallExtraPlugin(pluginBinDir, entry.Name); err != nil {
				log.Logger().Warnf("failed to upgrade plugin %s: %+v", entry.Name, err)
			}
		}
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d plugins failed to upgrade, see the FAILED lines above", failures, len(plan.Entries))
	}
	return nil
}

// executeEntry installs one declared plugin if needed and runs its hook
func (o *PluginOptions) executeEntry(pluginBinDir string, entry pluginPlanEntry) error {
	log.Logger().Infof("checking binary jx plugin %s version %s is installed", termcolor.ColorInfo(entry.Name), termcolor.ColorInfo(entry.Target))

	fileName, err := o.ensureInstalled(pluginBinDir, entry)
	if err != nil {
		return err
	}
	if entry.Hook == nil {
		return nil
	}

	c := &cmdrunner.Command{
		Name: fileName,
		Args: entry.Hook.hookArgs(o.Path),
	}
	_, err = o.CommandRunner(c)
	if err != nil {
		return fmt.Errorf("failed to run hook %q for plugin %s: %w", cmdrunner.CLI(c), entry.Name, err)
	}
	return nil
}

// ensureInstalled installs the entry's target version when it is not there yet. The plan is only a
// snapshot: another job sharing this home directory may have installed or replaced the plugin while
// this run was planning, so the directory is re-read immediately before installing.
func (o *PluginOptions) ensureInstalled(pluginBinDir string, entry pluginPlanEntry) (string, error) {
	installed, err := scanPluginDir(pluginBinDir)
	if err != nil {
		return "", err
	}
	if current := installed[entry.Name]; !sameVersions(current, entry.Installed) {
		log.Logger().Infof("plugin directory changed while planning plugin %s: now has versions %v, planned %v",
			termcolor.ColorInfo(entry.Name), current, entry.Installed)
		fmt.Fprintf(o.Out, recheckRowFormat, entry.Name, installedVersions(current), "re-read-the-plugin-directory")
	}
	if containsVersion(installed[entry.Name], entry.Target) {
		return installedPluginPath(pluginBinDir, entry.Name, entry.Target), nil
	}
	if entry.Plugin == nil {
		return "", fmt.Errorf("no plugin definition to install %s from", entry.Name)
	}

	path, err := o.InstallPlugin(*entry.Plugin, pluginBinDir)
	if err != nil {
		return "", fmt.Errorf("failed to ensure plugin is installed %s: %w", entry.Name, err)
	}
	return path, nil
}

// installedVersions renders an empty version list distinctly from a plugin that is absent
func installedVersions(versions []string) string {
	if len(versions) == 0 {
		return "(none)"
	}
	return fmt.Sprint(versions)
}

// sameVersions reports whether two sorted version lists hold the same versions
func sameVersions(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
