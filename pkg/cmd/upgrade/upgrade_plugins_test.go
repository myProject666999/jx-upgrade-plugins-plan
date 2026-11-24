package upgrade_test

import (
	"bytes"
	"testing"

	jxCore "github.com/jenkins-x/jx-api/v4/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx-helpers/v3/pkg/cmdrunner"
	"github.com/jenkins-x/jx/pkg/cmd/upgrade"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpgrade runs the cobra command against a temporary plugin directory and fake substitutes.
//
// It used to call cmd.Execute() with no substitutes at all, so on a developer machine it read the real
// plugin directory and tried to download and install plugins, which made it depend on the network and
// on whatever happened to be installed locally. The default installer is still what production uses;
// only this test swaps it out.
func TestUpgrade(t *testing.T) {
	t.Parallel()

	cmd, o := upgrade.NewCmdUpgradePlugins()
	o.PluginBinDir = t.TempDir()
	out := &bytes.Buffer{}
	o.Out = out
	o.DryRun = true
	o.InstallPlugin = func(jxCore.Plugin, string) (string, error) {
		t.Fatal("dry run must not install plugins")
		return "", nil
	}
	o.CommandRunner = func(*cmdrunner.Command) (string, error) {
		t.Fatal("dry run must not run hooks")
		return "", nil
	}

	require.NoError(t, cmd.Execute())
	assert.Contains(t, out.String(), "PLUGIN\tTARGET\tINSTALLED\tACTION")
}
