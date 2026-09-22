package cli

import (
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
)

func TestVersionCommand(t *testing.T) {
	code, stdout, stderr := runMain("version", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	env := decodeEnvelope(t, stdout)
	data, _ := env.Data.(map[string]any)
	if data["build"] != daemon.SelfBuild().ID() || data["guide"] != float64(guideVersion) || data["version"] == "" || data["go"] == "" {
		t.Errorf("version data = %v", data)
	}
	_, text, _ := runMain("version", "--format", "text")
	if !strings.HasPrefix(text, "lightspeed ") || !strings.Contains(text, daemon.SelfBuild().ID()) {
		t.Errorf("version text = %q", text)
	}
	for _, flag := range []string{"-v", "--version"} {
		if c, out, _ := runMain(flag, "--format", "json"); c != 0 || out != stdout {
			t.Errorf("lightspeed %s = exit %d %.100q, want the version command's answer", flag, c, out)
		}
	}
}

func TestVersionIsStampable(t *testing.T) {
	old := version
	version = "v9.9.9-test"
	defer func() { version = old }()
	if got := currentBuildInfo().Version; got != "v9.9.9-test" {
		t.Errorf("version = %q, want the stamped one", got)
	}
	if got := mcpVersion(); got != "v9.9.9-test" {
		t.Errorf("the MCP server reports %q, want the stamped version", got)
	}
}
