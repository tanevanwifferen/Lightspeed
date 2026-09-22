package serverdef

import (
	"slices"
	"testing"
)

func TestDeadCodeTableIsAcceptedBesideDefinitionsAndAlone(t *testing.T) {
	for name, src := range map[string]string{
		"alone": "schema_version = 1\n[dead_code]\nignore_names = [\"Handle\"]\n",
		"with one definition": "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 60\n" +
			"[dead_code]\nignore_names = [\"Handle\"]\n",
		"with several definitions": "schema_version = 1\n[servers.gopls.activation]\npriority = 60\n" +
			"[dead_code]\nignore_names = [\"Handle\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			frags, err := ParseFragments([]byte(src))
			if err != nil {
				t.Fatalf("a [dead_code] table must not be refused as an unknown key: %v", err)
			}
			if name == "alone" && len(frags) != 0 {
				t.Errorf("a file with only [dead_code] defines no server, got %d fragment(s)", len(frags))
			}
			if name != "alone" && len(frags) != 1 {
				t.Errorf("got %d fragment(s), want the one definition", len(frags))
			}
		})
	}
}

func TestParseDeadCodeConfig(t *testing.T) {
	cfg, err := ParseDeadCodeConfig([]byte("schema_version = 1\n[dead_code]\nignore_names = [\"Handle\", \"Test*\"]\nignore_paths = [\"gen/**\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.IgnoreNames, []string{"Handle", "Test*"}) || !slices.Equal(cfg.IgnorePaths, []string{"gen/**"}) {
		t.Errorf("config = %+v", cfg)
	}
	if _, err := ParseDeadCodeConfig([]byte("schema_version = 1\n[dead_code]\nignore_name = [\"x\"]\n")); err == nil {
		t.Error("a typo in a [dead_code] key must be an error, not an ignore that does nothing")
	}
	if _, err := ParseDeadCodeConfig([]byte("schema_version = 1\n[dead_code]\nignore_names = \"x\"\n")); err == nil {
		t.Error("a non-array ignore_names must be an error")
	}
	if cfg, err := ParseDeadCodeConfig([]byte("schema_version = 1\n")); err != nil || len(cfg.IgnoreNames) != 0 {
		t.Errorf("no table is an empty config, got %+v, %v", cfg, err)
	}
}

func TestLoadDeadCodeConfigMissingFileIsEmpty(t *testing.T) {
	cfg, err := LoadDeadCodeConfig(t.TempDir())
	if err != nil || cfg == nil || len(cfg.IgnoreNames) != 0 {
		t.Errorf("missing file: %+v, %v", cfg, err)
	}
}
