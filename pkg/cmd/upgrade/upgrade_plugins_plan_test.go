package upgrade

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/blang/semver"
	jxCore "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx/pkg/plugins"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInstaller records the plugins an upgrade asks to install, in order
type fakeInstaller struct {
	Calls []string
	// ErrByPlugin fails the install for the named plugin
	ErrByPlugin map[string]error
}

func (f *fakeInstaller) Install(plugin jxCore.Plugin, dir string) (string, error) {
	f.Calls = append(f.Calls, plugin.Spec.Name)
	if err, ok := f.ErrByPlugin[plugin.Spec.Name]; ok {
		return "", err
	}
	return filepath.Join(dir, plugin.Spec.Name+"-"+plugin.Spec.Version), nil
}

// fakeRunner records the hooks that were run, in order
type fakeRunner struct {
	Calls []string
	// ErrByCommand fails the hook whose command line contains the key
	ErrByCommand map[string]error
}

func (f *fakeRunner) Run(c *cmdrunner.Command) (string, error) {
	cli := cmdrunner.CLI(c)
	f.Calls = append(f.Calls, cli)
	for key, err := range f.ErrByCommand {
		if strings.Contains(cli, key) {
			return "", err
		}
	}
	return "", nil
}

// targetVersion the version this build intends for the named plugin, which is given without the "jx-" prefix
func targetVersion(t *testing.T, name string) string {
	t.Helper()

	for i := range plugins.Plugins {
		p := &plugins.Plugins[i]
		if p.Name == name {
			return p.Spec.Version
		}
	}
	t.Fatalf("plugin %s is not declared", name)
	return ""
}

// touchFiles creates empty files in the plugin directory, standing in for installed binaries
func touchFiles(t *testing.T, dir string, names ...string) {
	t.Helper()

	for _, name := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600), "failed to create fake plugin %s", name)
	}
}

// fakeExtraInstaller records extra plugins that execution asked to upgrade
type fakeExtraInstaller struct {
	Calls []string
	Err   error
}

func (f *fakeExtraInstaller) Install(dir, name string) (string, error) {
	f.Calls = append(f.Calls, name)
	return "", f.Err
}

// newTestOptions options pointed at a temporary plugin directory, with output captured
func newTestOptions(t *testing.T, dir string) (*PluginOptions, *bytes.Buffer, *fakeInstaller, *fakeRunner) {
	t.Helper()

	o := &PluginOptions{PluginBinDir: dir, Path: "/test/bin"}
	out := &bytes.Buffer{}
	o.Out = out
	installer := &fakeInstaller{ErrByPlugin: map[string]error{}}
	o.InstallPlugin = installer.Install
	o.InstallExtraPlugin = func(string, string) (string, error) {
		t.Fatal("extra plugins must not be looked up when there is no local source evidence")
		return "", nil
	}
	runner := &fakeRunner{ErrByCommand: map[string]error{}}
	o.CommandRunner = runner.Run
	return o, out, installer, runner
}

// planLines the non-empty plan output lines
func planLines(output string) []string {
	lines := make([]string, 0)
	for _, line := range strings.Split(output, "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// findPlanRow the plan row for the given plugin name, and the rest of the plan
func findPlanRow(t *testing.T, output, name string) string {
	t.Helper()

	for _, line := range planLines(output) {
		fields := strings.Split(line, "\t")
		if len(fields) == 4 && fields[0] == name {
			return line
		}
	}
	t.Fatalf("no plan row for plugin %s in:\n%s", name, output)
	return ""
}

// TestPlanEmptyDirectory covers a plugin directory with nothing installed: every declared plugin in
// scope needs installing, and execution installs each of them once with no hook outside --boot.
func TestPlanEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	o, _, installer, runner := newTestOptions(t, dir)

	require.NoError(t, o.Run())

	assert.Len(t, installer.Calls, len(plugins.Plugins), "every declared plugin is installed once")
	require.Len(t, runner.Calls, 1, "only the secret hook runs outside --boot")
	assert.True(t, strings.HasSuffix(runner.Calls[0], " plugins upgrade"), "the only hook is the secret one, got %v", runner.Calls)
}

// TestDryRunEmptyDirectory covers the read-only path: no plugin directory is created, no installs and no
// hooks happen, and the plan shows every declared plugin needing an install.
func TestDryRunEmptyDirectory(t *testing.T) {
	// point at a directory inside t.TempDir() that does not exist yet
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	_, err := os.Stat(dir)
	assert.True(t, os.IsNotExist(err), "dry run must not create the plugin directory")
	assert.Empty(t, installer.Calls, "dry run must not install plugins")
	assert.Empty(t, runner.Calls, "dry run must not run hooks")

	lines := planLines(out.String())
	require.True(t, len(lines) > 1, "expected a header and plan rows, got:\n%s", out.String())
	assert.Equal(t, "PLUGIN\tTARGET\tINSTALLED\tACTION", lines[0], "header")

	row := findPlanRow(t, out.String(), "jx-gitops")
	assert.Equal(t, fmt.Sprintf("jx-gitops\t%s\t\tinstall", targetVersion(t, "gitops")), row,
		"an uninstalled plugin with no hook has empty installed versions and the install action")
}

// TestDryRunTargetVersionInstalled covers a plugin directory that already holds the target versions:
// nothing has to be installed, though a plugin with a hook still has the hook to run.
func TestDryRunTargetVersionInstalled(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "jx-admin-"+targetVersion(t, "admin"), "jx-secret-"+targetVersion(t, "secret"))
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)

	adminTarget := targetVersion(t, "admin")
	row := findPlanRow(t, out.String(), "jx-admin")
	assert.Equal(t, fmt.Sprintf("jx-admin\t%s\t%s\tsatisfied", adminTarget, adminTarget), row,
		"a plugin with no hook at its target version is satisfied")

	// secret has a non-boot hook, so it still has work to do even though the version is there
	secretTarget := targetVersion(t, "secret")
	row = findPlanRow(t, out.String(), "jx-secret")
	assert.Equal(t, fmt.Sprintf("jx-secret\t%s\t%s\trun-hook", secretTarget, secretTarget), row,
		"an installed plugin whose hook applies is planned as run-hook, not satisfied")
}

// TestDryRunMultipleVersions covers a plugin directory holding several versions of the same plugin: the
// INSTALLED field lists them newest first and the target version wins regardless of the extra versions.
func TestDryRunMultipleVersions(t *testing.T) {
	dir := t.TempDir()
	gitopsTarget := targetVersion(t, "gitops")
	touchFiles(t, dir, "jx-gitops-1.0.0", "jx-gitops-"+gitopsTarget, "jx-gitops-1.10.0", "jx-gitops-1.9.0")
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true
	o.Boot = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)

	expectedVersions := strings.Join(semverSortedDesc(t, "1.0.0", gitopsTarget, "1.10.0", "1.9.0"), ",")
	row := findPlanRow(t, out.String(), "jx-gitops")
	assert.Equal(t, fmt.Sprintf("jx-gitops\t%s\t%s\trun-hook", gitopsTarget, expectedVersions), row,
		"versions are listed newest first and only the exact target version counts as installed for the hook case")
}

// TestDryRunPrereleaseVersion covers a pre-release file: it is a valid version, so it is listed, but it
// does not satisfy a release target.
func TestDryRunPrereleaseVersion(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "jx-test-0.4.15-dev")
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)
	row := findPlanRow(t, out.String(), "jx-test")
	assert.Equal(t, fmt.Sprintf("jx-test\t%s\t0.4.15-dev\tinstall", targetVersion(t, "test")), row,
		"a pre-release file is listed but does not satisfy the release target")
}

// TestDryRunThirdPartyPlugin covers plugins that are in the directory but not declared: they are listed
// sorted by name, all as unknown source, and never looked up or touched.
func TestDryRunThirdPartyPlugin(t *testing.T) {
	dir := t.TempDir()
	// deliberately created so that map iteration order would differ from sorted order
	touchFiles(t, dir, "jx-zebra-0.1.0", "jx-bar-2.0.0", "jx-alpha-3.0.0")
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)

	lines := planLines(out.String())
	extraIdx := -1
	for i, line := range lines {
		if line == "EXTRA-PLUGINS" {
			extraIdx = i
			break
		}
	}
	require.Greater(t, extraIdx, -1, "expected an EXTRA-PLUGINS section in:\n%s", out.String())

	expected := []string{
		"jx-alpha\tunknown-source\t3.0.0\tskip-unknown-source",
		"jx-bar\tunknown-source\t2.0.0\tskip-unknown-source",
		"jx-zebra\tunknown-source\t0.1.0\tskip-unknown-source",
	}
	actual := lines[extraIdx+1 : extraIdx+1+len(expected)]
	assert.Equal(t, expected, actual, "extra plugins are sorted by name and all have unknown source")
}

// TestExecuteSkipsUnknownSourceExtras covers execution of the extra plugin section: unknown source
// plugins are reported but never installed or looked up, and the run still succeeds.
func TestExecuteSkipsUnknownSourceExtras(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "jx-thirdparty-1.0.0", "jx-other-2.0.0")
	o, out, installer, _ := newTestOptions(t, dir)
	// newTestOptions fails the test if this is ever called for an unknown source plugin
	extra := &fakeExtraInstaller{}
	o.InstallExtraPlugin = extra.Install

	require.NoError(t, o.Run(), "unknown source extra plugins only warn, they do not fail the run")

	assert.NotContains(t, installer.Calls, "jx-thirdparty", "unknown source plugins are not installed")
	assert.NotContains(t, installer.Calls, "jx-other", "unknown source plugins are not installed")
	assert.Empty(t, extra.Calls, "unknown source plugins are not looked up")
	assert.Contains(t, out.String(), "jx-thirdparty\tunknown-source\t1.0.0\tskip-unknown-source")
}

// TestExecuteQueriesLatestForKnownSourceExtra covers the other extra plugin branch: an extra plugin with
// local upstream evidence is looked up at execution time, and a failure there only warns.
func TestExecuteQueriesLatestForKnownSourceExtra(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "jx-known-1.0.0")
	o, out, _, _ := newTestOptions(t, dir)
	extra := &fakeExtraInstaller{Err: fmt.Errorf("github is down")}
	o.InstallExtraPlugin = extra.Install
	_ = out

	// the evidence lookup has no case today, so this test drives the branch through the mapping from
	// evidence to plan entry rather than pretending the default lookup finds evidence
	target, action, evidence := extraPlan(evidenceUpstream)
	assert.Equal(t, targetQueryLatest, target)
	assert.Equal(t, actionQueryLatest, action)
	assert.Equal(t, evidenceUpstream, evidence)

	// and the default lookup really does report no evidence
	_, defaultAction, defaultEvidence := planExtra(dir, "jx-known")
	assert.Equal(t, actionSkipUnknownSource, defaultAction, "by default nothing proves an upstream source")
	assert.Equal(t, evidenceNone, defaultEvidence)

	// a lookup failure for a known source plugin only warns and does not change the exit status
	require.NoError(t, o.execute(dir, &pluginPlan{
		IncludeExtras: true,
		Extras:        []pluginPlanEntry{{Name: "jx-known", Target: targetQueryLatest, Action: actionQueryLatest}},
	}), "an extra plugin lookup failure does not fail the run")
	assert.Equal(t, []string{"jx-known"}, extra.Calls, "the known source plugin was looked up")
}

// TestExecuteClosingSummary covers what happens with no extra plugins and nothing installed: every
// declared plugin is installed, nothing fails, and the summary counts match the rows.
func TestExecuteClosingSummary(t *testing.T) {
	dir := t.TempDir()
	o, out, _, _ := newTestOptions(t, dir)

	require.NoError(t, o.Run())

	summary := ""
	for _, line := range planLines(out.String()) {
		if strings.HasPrefix(line, "SUMMARY\t") {
			summary = line
		}
	}
	require.NotEmpty(t, summary, "expected a SUMMARY line in:\n%s", out.String())
	assert.Contains(t, summary, fmt.Sprintf("install=%d", len(plugins.Plugins)-1),
		"every declared plugin except secret, which has a hook, plans as install")
	assert.Contains(t, summary, "install-and-run-hook=1", "the secret hook is the only install-and-run-hook")
	assert.Contains(t, summary, "satisfied=0")
	assert.Contains(t, summary, "skip-unknown-source=0")
}

// TestDryRunNonPluginFiles covers files in the plugin directory that are not plugin binaries: they are
// not part of the plan.
func TestDryRunNonPluginFiles(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "README", "jx-gitops", "jx-gitops-latest", "kubectl-1.0.0")
	o, out, _, _ := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	assert.NotContains(t, out.String(), "EXTRA-PLUGINS", "files that are not plugin binaries are not extra plugins")
	assert.NotContains(t, out.String(), "README")
	assert.NotContains(t, out.String(), "kubectl")
	// jx-gitops has no parseable version, so it must not be listed as an extra plugin either
	for _, line := range strings.Split(out.String(), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) > 1 && fields[1] == "unknown-source" {
			t.Fatalf("unexpected extra plugin row %q", line)
		}
	}
}

// TestDryRunMandatoryScope covers --mandatory: the declared plugins are all still planned, only the extra
// plugins are dropped.
func TestDryRunMandatoryScope(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "jx-thirdparty-1.0.0")
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true
	o.OnlyMandatory = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)
	assert.NotContains(t, out.String(), "EXTRA-PLUGINS", "--mandatory skips extra plugins")
	assert.NotContains(t, out.String(), "jx-thirdparty")

	declaredRows := 0
	for _, line := range planLines(out.String()) {
		fields := strings.Split(line, "\t")
		if len(fields) == 4 && plugins.PluginMap[fields[0]] != nil {
			declaredRows++
		}
	}
	assert.Equal(t, len(plugins.Plugins), declaredRows, "--mandatory does not narrow the declared plugins")
}

// TestDryRunBootScope covers --boot: exactly the five boot plugins are planned, with the gitops hook.
func TestDryRunBootScope(t *testing.T) {
	dir := t.TempDir()
	touchFiles(t, dir, "jx-thirdparty-1.0.0")
	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true
	o.Boot = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)
	assert.NotContains(t, out.String(), "EXTRA-PLUGINS", "--boot skips extra plugins")

	names := make([]string, 0, len(bootPluginNames))
	for _, line := range planLines(out.String()) {
		fields := strings.Split(line, "\t")
		if len(fields) == 4 && plugins.PluginMap[fields[0]] != nil {
			names = append(names, fields[0])
		}
	}
	assert.Equal(t, len(bootPluginNames), len(names), "only the boot plugins are planned, got %v", names)

	row := findPlanRow(t, out.String(), "jx-gitops")
	assert.Equal(t, fmt.Sprintf("jx-gitops\t%s\t\tinstall-and-run-hook", targetVersion(t, "gitops")), row,
		"gitops has the boot hook, so it is installed and then the hook runs")
	// secret is in the boot set, but its hook is the non-boot one, so under --boot it is a plain install
	row = findPlanRow(t, out.String(), "jx-secret")
	assert.Equal(t, fmt.Sprintf("jx-secret\t%s\t\tinstall", targetVersion(t, "secret")), row,
		"--boot plans secret but not its non-boot hook")
}

// TestPlanOrderIsDeclarationOrder covers the plan order: the declared plugins follow plugins.Plugins.
func TestPlanOrderIsDeclarationOrder(t *testing.T) {
	dir := t.TempDir()
	o, out, _, _ := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	var names []string
	for _, line := range planLines(out.String()) {
		fields := strings.Split(line, "\t")
		if len(fields) == 4 && plugins.PluginMap[fields[0]] != nil {
			names = append(names, fields[0])
		}
	}
	expected := make([]string, len(plugins.Plugins))
	for i := range plugins.Plugins {
		expected[i] = plugins.Plugins[i].Spec.Name
	}
	assert.Equal(t, expected, names, "plan order follows plugins.Plugins")
}

// TestExecuteInstallsAndRunsHooks covers execution: the target version is installed once, hooks run in
// plan order with the documented arguments, and hooks whose plugin is already satisfied still run.
func TestExecuteInstallsAndRunsHooks(t *testing.T) {
	dir := t.TempDir()
	// secret is already at the target version, so only its hook should run
	touchFiles(t, dir, "jx-secret-"+targetVersion(t, "secret"))
	o, out, installer, runner := newTestOptions(t, dir)

	require.NoError(t, o.Run())

	assert.Equal(t, len(plugins.Plugins)-1, len(installer.Calls),
		"every declared plugin except the one already at target is installed once")
	assert.NotContains(t, installer.Calls, "jx-secret", "an already satisfied plugin is not installed again")
	assert.Equal(t, plugins.Plugins[0].Spec.Name, installer.Calls[0], "installs follow the plan order")

	secretTarget := targetVersion(t, "secret")
	assert.Equal(t, []string{
		filepath.Join(dir, "jx-secret-"+secretTarget) + " plugins upgrade",
	}, runner.Calls,
		"outside --boot the secret hook runs with 'plugins upgrade' and the installed binary path")

	assert.Contains(t, out.String(), "SUMMARY\t", "the plan is printed before execution")
}

// TestExecuteBootRunsGitopsHook covers the boot hook: only gitops runs a hook and it gets --path.
func TestExecuteBootRunsGitopsHook(t *testing.T) {
	dir := t.TempDir()
	o, _, installer, runner := newTestOptions(t, dir)
	o.Boot = true
	o.Path = "/custom/bin"

	require.NoError(t, o.Run())

	assert.Equal(t, []string{
		filepath.Join(dir, "jx-gitops-"+targetVersion(t, "gitops")) + " plugin upgrade --path /custom/bin",
	}, runner.Calls, "in --boot mode only gitops runs a hook, and it receives --path")
	assert.ElementsMatch(t, []string{"jx-gitops", "jx-health", "jx-secret", "jx-verify", "jx-changelog"}, installer.Calls)
}

// TestExecuteContinuesAfterFailure covers failure handling: a failing plugin does not stop the rest of
// the plan, and the returned error is non-nil so the command exits non-zero.
func TestExecuteContinuesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	o, out, installer, runner := newTestOptions(t, dir)
	installer.ErrByPlugin["jx-admin"] = fmt.Errorf("network is down")

	err := o.Run()
	require.Error(t, err, "a failed plugin makes the command exit non-zero")

	assert.Contains(t, err.Error(), "1 of", "the error summarises how many plugins failed")
	assert.Len(t, installer.Calls, len(plugins.Plugins), "every declared plugin was still attempted")
	assert.NotEmpty(t, runner.Calls, "the secret hook still ran after the admin failure")
	assert.Contains(t, out.String(), "FAILED\tjx-admin", "the failure is reported in the output")
}

// TestExecuteHookFailureIsReported covers a failing hook: it counts as a failure for its plugin but the
// other plugins still run.
func TestExecuteHookFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	o, out, installer, runner := newTestOptions(t, dir)
	runner.ErrByCommand["plugins upgrade"] = fmt.Errorf("hook exploded")

	err := o.Run()
	require.Error(t, err)

	assert.Len(t, installer.Calls, len(plugins.Plugins))
	assert.Equal(t, 1, len(runner.Calls))
	assert.Contains(t, out.String(), "FAILED\tjx-secret")
}

// TestExecuteRechecksDirectoryBeforeInstall covers the plan/execution gap: versions that appear after
// planning are reported instead of silently overwritten, and the plugin is not installed again.
func TestExecuteRechecksDirectoryBeforeInstall(t *testing.T) {
	dir := t.TempDir()
	o, out, installer, runner := newTestOptions(t, dir)
	o.Boot = true

	plan, err := o.createPlan(dir)
	require.NoError(t, err)

	// another job sharing this home directory installs gitops while we are between plan and execute
	gitopsTarget := targetVersion(t, "gitops")
	touchFiles(t, dir, "jx-gitops-"+gitopsTarget)

	require.NoError(t, o.execute(dir, plan))

	assert.Contains(t, out.String(), "RECHECK\tjx-gitops", "the directory change is reported")
	assert.NotContains(t, installer.Calls, "jx-gitops", "the plugin was installed by the other job, so it is not installed again")
	assert.Contains(t, installer.Calls, "jx-health", "the rest of the plan still runs")
	// gitops is now satisfied, so its hook still runs against the newly installed binary
	assert.Equal(t, []string{filepath.Join(dir, "jx-gitops-"+gitopsTarget) + " plugin upgrade --path /test/bin"}, runner.Calls)
}

// TestDryRunHasNoSideEffects covers the strict read-only promise: with no directory, no installs, no
// hooks and no directory creation.
func TestDryRunHasNoSideEffects(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "plugin-bin")
	o, _, installer, runner := newTestOptions(t, dir)
	o.DryRun = true
	o.Boot = true

	require.NoError(t, o.Run())

	entries, err := os.ReadDir(base)
	require.NoError(t, err)
	assert.Empty(t, entries, "dry run leaves the filesystem untouched")
	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)
}

// TestDryRunDoesNotTouchSymlinks covers the symlink case: a symlink in the plugin directory is neither
// followed nor changed by a dry run.
func TestDryRunDoesNotTouchSymlinks(t *testing.T) {
	dir := t.TempDir()
	linkTarget := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(linkTarget, []byte("data"), 0o600))
	link := filepath.Join(dir, "jx-gitops-9.9.9")
	require.NoError(t, os.Symlink(linkTarget, link))

	o, out, installer, runner := newTestOptions(t, dir)
	o.DryRun = true

	require.NoError(t, o.Run())

	assert.Empty(t, installer.Calls)
	assert.Empty(t, runner.Calls)

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSymlink, "the symlink is still a symlink")

	targetInfo, err := os.Stat(linkTarget)
	require.NoError(t, err)
	assert.Equal(t, int64(len("data")), targetInfo.Size(), "the symlink target is unchanged")

	// the symlink name still parses as a version and is reported as installed
	assert.Contains(t, findPlanRow(t, out.String(), "jx-gitops"), "9.9.9", "the symlink's version is listed")
}

// TestPlanScopeTableIsComplete covers the scope table: every flag combination is defined and unknown
// ones do not widen the plan.
func TestPlanScopeTableIsComplete(t *testing.T) {
	for _, name := range []string{scopeDefault, scopeMandatory, scopeBoot} {
		scope, ok := pluginScopeTable[name]
		require.True(t, ok, "scope %s is missing from the table", name)
		assert.NotEmpty(t, scope.Plugins, "scope %s covers no plugins", name)
	}
	assert.Len(t, pluginScopeTable[scopeBoot].Plugins, len(bootPluginNames), "the boot scope is the five boot plugins")
	assert.Len(t, pluginScopeTable[scopeDefault].Plugins, len(plugins.Plugins), "the default scope is every declared plugin")
	assert.Len(t, pluginScopeTable[scopeMandatory].Plugins, len(plugins.Plugins), "--mandatory covers every declared plugin")
}

// TestHookTable covers the hook table: one hook per plugin and mode, with the documented arguments.
func TestHookTable(t *testing.T) {
	gitops := hookFor("gitops", true)
	require.NotNil(t, gitops, "gitops has a boot hook")
	assert.Equal(t, []string{"plugin", "upgrade", "--path", "/x"}, gitops.hookArgs("/x"))

	assert.Nil(t, hookFor("gitops", false), "the gitops hook only applies in --boot mode")
	assert.Nil(t, hookFor("secret", true), "the secret hook only applies outside --boot mode")

	secret := hookFor("secret", false)
	require.NotNil(t, secret, "secret has a non-boot hook")
	assert.Equal(t, []string{"plugins", "upgrade"}, secret.hookArgs("/x"))

	assert.Nil(t, hookFor("admin", false), "plugins without a hook have none")
}

// semverSortedDesc sorts the given versions newest first, so tests do not restate the comparator
func semverSortedDesc(t *testing.T, versions ...string) []string {
	t.Helper()

	parsed := make([]semver.Version, 0, len(versions))
	for _, v := range versions {
		version, err := semver.Parse(v)
		require.NoError(t, err, "failed to parse test version %s", v)
		parsed = append(parsed, version)
	}
	sort.Sort(sort.Reverse(semver.Versions(parsed)))

	sorted := make([]string, len(parsed))
	for i, version := range parsed {
		sorted[i] = version.String()
	}
	return sorted
}
