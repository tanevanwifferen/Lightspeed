package index

import (
	"reflect"
	"testing"
)

func TestTokenize(t *testing.T) {
	cases := map[string][]string{
		"parseConfig":         {"parse", "config"},
		"ParseConfigFile":     {"parse", "config", "file"},
		"parse_config":        {"parse", "config"},
		"parse-config.file":   {"parse", "config", "file"},
		"HTTPServer_v2":       {"http", "server", "v", "2"},
		"getHTTPResponseCode": {"get", "http", "response", "code"},
		"ID":                  {"id"},
		"userID":              {"user", "id"},
		"XMLHttpRequest":      {"xml", "http", "request"},
		"__init__":            {"init"},
		"a1b2":                {"a", "1", "b", "2"},
		"Server.Handle":       {"server", "handle"},
		"func(a int) string":  {"func", "a", "int", "string"},
		"名前Name":              {"名前", "name"},
		"Größe_ÄÖ":            {"größe", "äö"},
		"   ":                 nil,
		"":                    nil,
	}
	for in, want := range cases {
		if got := Tokenize(in); !reflect.DeepEqual(got, want) {
			t.Errorf("Tokenize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalise(t *testing.T) {
	for in, want := range map[string]string{
		"parse_config":  "parseconfig",
		"ParseConfig":   "parseconfig",
		"parse config":  "parseconfig",
		"(*Server).Run": "serverrun",
		"":              "",
	} {
		if got := normalise(in); got != want {
			t.Errorf("normalise(%q) = %q, want %q", in, got, want)
		}
	}
}
