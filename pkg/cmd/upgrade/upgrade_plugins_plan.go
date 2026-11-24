package upgrade

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/blang/semver"
	jxCore "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"

	"github.com/jenkins-x/jx-helpers/v3/pkg/homedir"
	"github.com/jenkins-x/jx-logging/v3/pkg/log"
	"github.com/jenkins-x/jx/pkg/plugins"
)

// pluginPlanAction is the action planned for a single plugin
type pluginPlanAction string

const (
	// actionSatisfied the target version is already installed and no hook applies
	actionSatisfied pluginPlanAction = "satisfied"
	// actionInstall the target version needs to be installed
	actionInstall pluginPlanAction = "install"
	// actionInstallAndRunHook the target version needs to be installed and then a hook is run
	actionInstallAndRunHook pluginPlanAction = "install-and-run-hook"
	// actionRunHook the target version is already installed but a hook still applies
	actionRunHook pluginPlanAction = "run-hook"
	// actionQueryLatest an extra plugin whose local files point at an upstream release; the latest
	// release is looked up at execution time
	actionQueryLatest pluginPlanAction = "query-latest-at-execution"
	// actionSkipUnknownSource an extra plugin with no local evidence of its upstream source; it is
	// listed but not touched
	actionSkipUnknownSource pluginPlanAction = "skip-unknown-source"

	// targetUnknownSource the TARGET column for an extra plugin with no local provenance evidence
	targetUnknownSource = "unknown-source"
	// targetQueryLatest the TARGET column for an extra plugin with local upstream provenance evidence
	targetQueryLatest = "query-latest-at-execution"

	// planRecheckNote tells the reader that execution re-reads the plugin directory before installing
	planRecheckNote = "installs-are-re-checked-against-the-plugin-directory-at-execution"

	// pluginRowFormat the fixed field order of a plan row: name, target, installed versions, action
	pluginRowFormat = "%s\t%s\t%s\t%s\n"

	// recheckRowFormat a plan row that differed from the plugin directory at execution time
	recheckRowFormat = "RECHECK\t%s\t%s\t%s\n"
	// failedRowFormat a plugin which failed while executing the plan
	failedRowFormat = "FAILED\t%s\t%v\n"
)

// planActionOrder the fixed order actions are counted and rendered in
var planActionOrder = []pluginPlanAction{
	actionSatisfied,
	actionInstall,
	actionInstallAndRunHook,
	actionRunHook,
	actionQueryLatest,
	actionSkipUnknownSource,
}

// pluginPlanEntry one plugin in an upgrade plan
type pluginPlanEntry struct {
	// Name the plugin binary name, for example "jx-gitops"
	Name string
	// Target the version to end up with; for extra plugins this may be a source marker instead
	Target string
	// Installed the versions found in the plugin directory, newest first
	Installed []string
	// Action what execution will do for this plugin
	Action pluginPlanAction
	// Plugin the declared plugin to install, nil for extra plugins
	Plugin *jxCore.Plugin
	// Hook the hook to run after ensuring the plugin is installed, if any
	Hook *pluginHook
}

// pluginPlan the full upgrade plan, in a fixed, diffable order
type pluginPlan struct {
	// Entries the declared plugins in plugins.Plugins declaration order
	Entries []pluginPlanEntry
	// Extras the plugins found in the plugin directory but not declared, sorted by name
	Extras []pluginPlanEntry
	// IncludeExtras whether extra plugins are part of the plan at all
	IncludeExtras bool
	// Counts how many entries carry each action
	Counts map[pluginPlanAction]int
}

// createPlan reads the plugin directory and works out what execution would do, in the order it would
// do it. It never creates the directory, writes files, follows symlinks or runs commands; reading the
// plugin directory is its only side effect free input.
func (o *PluginOptions) createPlan(pluginBinDir string) (*pluginPlan, error) {
	installed, err := scanPluginDir(pluginBinDir)
	if err != nil {
		return nil, err
	}
	scope := o.scope()
	plan := &pluginPlan{IncludeExtras: scope.IncludeExtras, Counts: map[pluginPlanAction]int{}}

	for i := range plugins.Plugins {
		plugin := &plugins.Plugins[i]
		if !scope.Plugins[plugin.Name] {
			continue
		}
		entry := pluginPlanEntry{
			Name:      plugin.Spec.Name,
			Target:    plugin.Spec.Version,
			Installed: installed[plugin.Spec.Name],
			Plugin:    plugin,
			Hook:      hookFor(plugin.Name, o.Boot),
		}
		entry.Action = entry.plannedAction()
		plan.Entries = append(plan.Entries, entry)
	}

	if scope.IncludeExtras {
		for name, versions := range installed {
			if plugins.PluginMap[name] != nil {
				continue
			}
			entry := pluginPlanEntry{Name: name, Installed: versions}
			var evidence upstreamEvidence
			entry.Target, entry.Action, evidence = planExtra(pluginBinDir, name)
			log.Logger().Debugf("extra plugin %s: %s", name, evidence)
			plan.Extras = append(plan.Extras, entry)
		}
		// the directory scan returns a map, so sort to keep the plan and its logs stable
		sort.Slice(plan.Extras, func(i, j int) bool { return plan.Extras[i].Name < plan.Extras[j].Name })
	}

	plan.count()
	return plan, nil
}

// plannedAction works out the action for a declared plugin from what is installed and whether a hook applies
func (e pluginPlanEntry) plannedAction() pluginPlanAction {
	satisfied := containsVersion(e.Installed, e.Target)
	switch {
	case e.Hook == nil && satisfied:
		return actionSatisfied
	case e.Hook == nil:
		return actionInstall
	case satisfied:
		return actionRunHook
	default:
		return actionInstallAndRunHook
	}
}

// count totals the actions in the plan
func (p *pluginPlan) count() {
	p.Counts = map[pluginPlanAction]int{}
	for _, entry := range p.Entries {
		p.Counts[entry.Action]++
	}
	for _, entry := range p.Extras {
		p.Counts[entry.Action]++
	}
}

// Render renders the plan with a fixed field order and tab separators so that it can be read by people
// and asserted with a plain text diff
func (p *pluginPlan) Render(w io.Writer) {
	fmt.Fprintln(w, "PLUGIN\tTARGET\tINSTALLED\tACTION")
	for _, entry := range p.Entries {
		entry.writeTo(w)
	}
	if p.IncludeExtras && len(p.Extras) > 0 {
		fmt.Fprintln(w, "EXTRA-PLUGINS")
		for _, entry := range p.Extras {
			entry.writeTo(w)
		}
	}
	fmt.Fprintf(w, "NOTE\t%s\n", planRecheckNote)
	fmt.Fprintln(w, p.summaryLine())
}

func (e pluginPlanEntry) writeTo(w io.Writer) {
	fmt.Fprintf(w, pluginRowFormat, e.Name, e.Target, strings.Join(e.Installed, ","), e.Action)
}

// summaryLine renders the action counts in the fixed planActionOrder
func (p *pluginPlan) summaryLine() string {
	fields := make([]string, 0, len(planActionOrder)+1)
	fields = append(fields, "SUMMARY")
	for _, action := range planActionOrder {
		fields = append(fields, fmt.Sprintf("%s=%d", action, p.Counts[action]))
	}
	return strings.Join(fields, "\t")
}

// scanPluginDir returns the versions found for each plugin binary name in the plugin directory, newest
// first. A missing directory yields an empty result rather than an error so that a dry-run against a
// fresh home directory stays read only.
func scanPluginDir(dir string) (map[string][]string, error) {
	versions := map[string][]string{}

	file, err := os.Open(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return versions, nil
		}
		return nil, fmt.Errorf("failed to read plugin dir %s: %w", dir, err)
	}
	defer file.Close()

	files, err := file.Readdirnames(0)
	if err != nil {
		return nil, fmt.Errorf("failed to read plugin dir %s: %w", dir, err)
	}

	parsed := map[string][]semver.Version{}
	for _, name := range files {
		pluginName, version, ok := splitPluginBinaryName(name)
		if !ok {
			continue
		}
		parsed[pluginName] = append(parsed[pluginName], version)
	}
	for name, found := range parsed {
		sort.Sort(sort.Reverse(semver.Versions(found)))
		sorted := make([]string, len(found))
		for i, version := range found {
			sorted[i] = version.String()
		}
		versions[name] = sorted
	}
	return versions, nil
}

// splitPluginBinaryName splits a file name like "jx-gitops-1.4.24" or "jx-test-0.4.15-dev" into the
// plugin name and its version. Only names whose suffix is a valid semantic version count, which is the
// same set FindStandardPlugin is able to sort. Names are split on the last separator which leaves a
// parsed version, so plugin names containing dashes keep working.
func splitPluginBinaryName(fileName string) (string, semver.Version, bool) {
	if !strings.HasPrefix(fileName, "jx-") {
		return "", semver.Version{}, false
	}
	for i := len(fileName) - 1; i >= 0; i-- {
		if fileName[i] != '-' {
			continue
		}
		version, err := semver.Parse(fileName[i+1:])
		if err != nil {
			continue
		}
		name := fileName[:i]
		if len(name) <= len("jx-") {
			return "", semver.Version{}, false
		}
		return name, version, true
	}
	return "", semver.Version{}, false
}

// containsVersion reports whether the given version is in the sorted version list
func containsVersion(versions []string, version string) bool {
	for _, v := range versions {
		if v == version {
			return true
		}
	}
	return false
}

// upstreamEvidence why an extra plugin is or is not treated as coming from an upstream release
type upstreamEvidence string

const (
	// evidenceNone nothing in the plugin directory says where the plugin came from
	evidenceNone upstreamEvidence = "no local source evidence"
	// evidenceUpstream a local file records an upstream release for the plugin
	evidenceUpstream upstreamEvidence = "local file records an upstream release"
)

// planExtra decides the target and action for an extra plugin from local evidence only. A dry run must
// not look up the network, so an extra plugin with no evidence is reported and skipped, even if its name
// happens to exist upstream.
func planExtra(pluginBinDir, name string) (string, pluginPlanAction, upstreamEvidence) {
	return extraPlan(findUpstreamEvidence(pluginBinDir, name))
}

// extraPlan maps local evidence to the plan fields for an extra plugin
func extraPlan(evidence upstreamEvidence) (string, pluginPlanAction, upstreamEvidence) {
	if evidence == evidenceNone {
		return targetUnknownSource, actionSkipUnknownSource, evidence
	}
	return targetQueryLatest, actionQueryLatest, evidence
}

// findUpstreamEvidence looks for a local record of where a plugin came from.
//
// Neither extensions.EnsurePluginInstalled nor plugins.InstallStandardPlugin records where a binary came
// from, so today nothing in the plugin directory proves an upstream origin and every extra plugin is
// reported as unknown source. This is the single seam to extend if local provenance is ever recorded;
// a dry-run must never guess by querying the network instead.
func findUpstreamEvidence(_, _ string) upstreamEvidence {
	return evidenceNone
}

// installedPluginPath the path EnsurePluginInstalled returns for a plugin at the given version
func installedPluginPath(dir, name, version string) string {
	return filepath.Join(dir, fmt.Sprintf("%s-%s", name, version))
}

// readOnlyPluginBinDir the default plugin directory path without creating it, for dry runs.
// homedir.PluginBinDir is what homedir.DefaultPluginBinDir builds on, so this matches its result.
func readOnlyPluginBinDir() string {
	root := os.Getenv("JX3_HOME")
	if root == "" {
		root = filepath.Join(homedir.HomeDir(), ".jx3")
	}
	return filepath.Join(root, "plugins", "bin")
}
