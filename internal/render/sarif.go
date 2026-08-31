package render

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// SARIF 2.1.0. Only the subset of the format that diagnostics need is
// modelled: a single run, one driver, rules derived from the servers'
// (source, code) pairs, and one physical location per result. Every
// property name and every enumerated value below is from the OASIS
// SARIF 2.1.0 specification.
const (
	sarifVersion = "2.1.0"
	sarifSchema  = "https://raw.githubusercontent.com/oasis-tcs/sarif-spec/master/Schemata/sarif-schema-2.1.0.json"

	// sarifDriverName is the tool name consumers key on.
	sarifDriverName = "lightspeed"
	sarifDriverURI  = "https://github.com/tanevanwifferen/Lightspeed"

	// sarifRootBaseID names the originalUriBaseIds entry that result
	// URIs are relative to when Options.Root is set.
	sarifRootBaseID = "SRCROOT"

	// sarifColumnKind is declared explicitly rather than left to the
	// consumer's default, because LSP columns are UTF-16 code units and
	// that is what we emit — untranslated, so nothing can be lost.
	sarifColumnKind = "utf16CodeUnits"
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool               sarifTool                        `json:"tool"`
	ColumnKind         string                           `json:"columnKind"`
	OriginalURIBaseIDs map[string]sarifArtifactLocation `json:"originalUriBaseIds,omitempty"`
	Invocations        []sarifInvocation                `json:"invocations,omitempty"`
	Results            []sarifResult                    `json:"results"`
	Properties         *sarifRunProperties              `json:"properties,omitempty"`
}

// sarifRunProperties is a SARIF property bag carrying the same
// truncation facts the JSON envelope reports, so a SARIF consumer is
// never silently handed a partial run either.
type sarifRunProperties struct {
	Truncated bool `json:"truncated"`
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Limit     int  `json:"limit,omitempty"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	InformationURI string      `json:"informationUri,omitempty"`
	Version        string      `json:"version,omitempty"`
	Rules          []sarifRule `json:"rules,omitempty"`
}

type sarifRule struct {
	ID               string        `json:"id"`
	ShortDescription *sarifMessage `json:"shortDescription,omitempty"`
}

type sarifInvocation struct {
	ExecutionSuccessful        bool                `json:"executionSuccessful"`
	ToolExecutionNotifications []sarifNotification `json:"toolExecutionNotifications,omitempty"`
}

type sarifNotification struct {
	Level   string       `json:"level"`
	Message sarifMessage `json:"message"`
}

type sarifResult struct {
	RuleID     string                 `json:"ruleId,omitempty"`
	RuleIndex  *int                   `json:"ruleIndex,omitempty"`
	Level      string                 `json:"level"`
	Message    sarifMessage           `json:"message"`
	Locations  []sarifLocation        `json:"locations"`
	Properties *sarifResultProperties `json:"properties,omitempty"`
}

// sarifResultProperties carries a diagnostic's LSP tags. SARIF has no
// first-class tag on a result, but `properties.tags` is the conventional
// place consumers look for them.
type sarifResultProperties struct {
	Tags []string `json:"tags,omitempty"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId,omitempty"`
}

type sarifRegion struct {
	StartLine   int                   `json:"startLine"`
	StartColumn int                   `json:"startColumn"`
	EndLine     int                   `json:"endLine"`
	EndColumn   int                   `json:"endColumn"`
	Snippet     *sarifArtifactContent `json:"snippet,omitempty"`
}

type sarifArtifactContent struct {
	Text string `json:"text"`
}

// diagnosticsSARIF renders diagnostics as a SARIF 2.1.0 log. The output
// is not wrapped in the lightspeed envelope: a SARIF consumer expects
// SARIF, and truncation is reported inside the log instead.
func diagnosticsSARIF(w io.Writer, ds DiagnosticSet, opts Options) error {
	kept, cut := truncate(ds.Diagnostics, opts.Limit)
	total := ds.total()

	var (
		rules     []sarifRule
		ruleIndex = map[string]int{}
		results   = make([]sarifResult, 0, len(kept))
	)
	for _, d := range kept {
		id := d.ruleID()
		idx, seen := ruleIndex[id]
		if !seen {
			idx = len(rules)
			ruleIndex[id] = idx
			rules = append(rules, sarifRule{ID: id, ShortDescription: shortDescription(d)})
		}
		result := sarifResult{
			RuleID:    id,
			RuleIndex: &idx,
			Level:     d.Severity.sarifLevel(),
			Message:   sarifMessage{Text: d.Message},
			Locations: []sarifLocation{{
				PhysicalLocation: sarifPhysicalLocation{
					ArtifactLocation: artifactLocation(d.URI, d.Path, opts),
					Region:           sarifRegionFor(d),
				},
			}},
		}
		if len(d.Tags) > 0 {
			result.Properties = &sarifResultProperties{Tags: d.Tags}
		}
		results = append(results, result)
	}

	run := sarifRun{
		Tool: sarifTool{Driver: sarifDriver{
			Name:           sarifDriverName,
			InformationURI: sarifDriverURI,
			Version:        opts.ToolVersion,
			Rules:          rules,
		}},
		ColumnKind: sarifColumnKind,
		Results:    results,
	}
	if opts.Root != "" {
		run.OriginalURIBaseIDs = map[string]sarifArtifactLocation{
			sarifRootBaseID: {URI: dirURI(opts.Root)},
		}
	}

	notices := noticesFor("diagnostics", len(kept), total, opts, cut)
	if len(notices) > 0 {
		inv := sarifInvocation{ExecutionSuccessful: true}
		for _, n := range notices {
			inv.ToolExecutionNotifications = append(inv.ToolExecutionNotifications, sarifNotification{
				Level:   "warning",
				Message: sarifMessage{Text: n},
			})
		}
		run.Invocations = []sarifInvocation{inv}
	}
	if cut || ds.Truncated {
		run.Properties = &sarifRunProperties{
			Truncated: true,
			Count:     len(kept),
			Total:     total,
			Limit:     opts.Limit,
		}
	}

	log := sarifLog{Schema: sarifSchema, Version: sarifVersion, Runs: []sarifRun{run}}
	enc := json.NewEncoder(w)
	if opts.Indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(log); err != nil {
		return Errorf(CodeIOError, "writing sarif: %v", err)
	}
	return nil
}

// shortDescription gives a rule a human label. A server tells us the
// code but never what it means, so the first diagnostic's own source
// and code is the most honest description available.
func shortDescription(d Diagnostic) *sarifMessage {
	if d.Source == "" && d.Code == "" {
		return nil
	}
	text := strings.TrimSpace(d.Source + " " + d.Code)
	return &sarifMessage{Text: text}
}

// artifactLocation places a file in the run: relative to SRCROOT when
// Options.Root contains it, an absolute file:// URI otherwise.
func artifactLocation(uri protocol.DocumentURI, path string, opts Options) sarifArtifactLocation {
	if opts.Root != "" && path != "" {
		if rel, err := filepath.Rel(opts.Root, path); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return sarifArtifactLocation{URI: filepath.ToSlash(rel), URIBaseID: sarifRootBaseID}
		}
	}
	return sarifArtifactLocation{URI: string(uri)}
}

// dirURI renders a directory as a file:// URI with the trailing slash
// that SARIF requires of a uriBaseId.
func dirURI(dir string) string {
	uri := string(protocol.URIFromPath(dir))
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	return uri
}

// sarifRegionFor converts a diagnostic's range to a SARIF region.
//
// SARIF lines and columns are 1-based and endColumn is exclusive — "the
// column number of the character following the end of the region" —
// which is exactly LSP's exclusive end, so the conversion is +1 on both
// columns with no re-encoding. The run declares columnKind
// utf16CodeUnits to say so.
func sarifRegionFor(d Diagnostic) *sarifRegion {
	r := &sarifRegion{
		StartLine:   int(d.Range.Start.Line) + 1,
		StartColumn: int(d.Range.Start.Character) + 1,
		EndLine:     int(d.Range.End.Line) + 1,
		EndColumn:   int(d.Range.End.Character) + 1,
	}
	if d.Text != "" {
		r.Snippet = &sarifArtifactContent{Text: d.Text}
	}
	return r
}
