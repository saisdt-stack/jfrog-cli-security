package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jfrog/jfrog-cli-security/utils"
	"github.com/jfrog/jfrog-cli-security/utils/formats"
)

// writeSummaryDataFile writes a recorded ScanCommandResultSummary to a temp file, mirroring
// what commandsummary.CommandSummary.Record produces on disk, so loadContent can read it back.
func writeSummaryDataFile(t *testing.T, content ScanCommandResultSummary) string {
	t.Helper()
	data, err := json.Marshal(content)
	require.NoError(t, err)
	filePath := filepath.Join(t.TempDir(), string(content.ResultType)+".json")
	require.NoError(t, os.WriteFile(filePath, data, 0600))
	return filePath
}

func TestGenerateActionsCurationSectionMarkdown(t *testing.T) {
	actions := func(attributed bool, entries ...formats.CuratedAction) *formats.CuratedActions {
		return &formats.CuratedActions{Attributed: attributed, Actions: entries}
	}
	tests := []struct {
		name string
		data []formats.ResultsSummary
		// wantEmpty: the section must be "" (not a newline, not an empty block) - GenerateMarkdownFromFiles appends it.
		wantEmpty       bool
		wantContains    []string
		wantNotContains []string
	}{
		{name: "verify when there is no data then nothing is rendered", data: nil, wantEmpty: true},
		{
			name: "verify when only package-curation data is present then nothing is rendered",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{
				{Target: "npm-project", CuratedPackages: &formats.CuratedPackages{PackageCount: 1}},
			}}},
			wantEmpty: true,
		},
		{
			name: "verify when every scan was attributed then the Parent column is rendered",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{{
				Target: ".github/workflows/ci.yml",
				CuratedActions: actions(true,
					formats.CuratedAction{Action: "actions/checkout", Ref: "v4", Status: "Approved"},
					formats.CuratedAction{Action: "some-org/transitive-action", Ref: "v1", Parent: "github/codeql-action@v3", Status: "Rejected", Notes: "policy failure"},
				),
			}}}},
			wantContains: []string{
				"GitHub Actions Curation", "| Action | Ref | Parent | Status | Notes |",
				"actions/checkout", "Approved",
				"some-org/transitive-action", "github/codeql-action@v3", "Rejected", "policy failure",
			},
			wantNotContains: []string{"Not covered"},
		},
		{
			name: "verify when no scan was attributed then the Parent column is omitted",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{{
				Target: "/home/runner/work/_actions",
				CuratedActions: actions(false,
					formats.CuratedAction{Action: "actions/checkout", Ref: "v4", Status: "Approved"},
					formats.CuratedAction{Action: "some-org/some-action", Ref: "v1", Status: "Rejected", Notes: "policy failure"},
				),
			}}}},
			wantContains:    []string{"GitHub Actions Curation", "| Action | Ref | Status | Notes |", "actions/checkout", "policy failure"},
			wantNotContains: []string{"Parent"},
		},
		{
			// Two recorded runs, which is the shape loadContent produces - one summary per data
			// file, each carrying the single scan NewCurationActionsSummary emits. One
			// unattributed run is enough: a single table cannot honestly caption both.
			name: "verify when attribution is mixed then the Parent column is dropped for the whole table",
			data: []formats.ResultsSummary{
				{Scans: []formats.ScanSummary{{Target: "ci.yml", CuratedActions: actions(true, formats.CuratedAction{Action: "a/b", Ref: "v1", Parent: "c/d@v2", Status: "Approved"})}}},
				{Scans: []formats.ScanSummary{{Target: "_actions", CuratedActions: actions(false, formats.CuratedAction{Action: "e/f", Ref: "v3", Status: "Approved"})}}},
			},
			wantContains:    []string{"| Action | Ref | Status | Notes |"},
			wantNotContains: []string{"c/d@v2"},
		},
		{
			// A markdown table ends only at a blank line, so a caveat on the very next line would
			// render as one more table row; the blank line comes from this function.
			name: "verify when a local composite action was declared then the section carries the caveat naming it after a blank line",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{{
				CuratedActions: &formats.CuratedActions{
					Attributed:            true,
					Actions:               []formats.CuratedAction{{Action: "actions/checkout", Ref: "v4", Status: "Approved"}},
					LocalCompositeActions: []formats.LocalCompositeAction{{Path: "./.github/actions/setup"}},
				},
			}}}},
			wantContains: []string{"Not covered", "./.github/actions/setup", "actions/checkout", "|\n\nNot covered"},
		},
		{
			name: "verify when a scan was not attributed then the section carries the unconditional caveat after a blank line",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{{
				CuratedActions: actions(false, formats.CuratedAction{Action: "actions/checkout", Ref: "v4", Status: "Approved"}),
			}}}},
			wantContains: []string{"Local composite actions (uses: ./...) are not curated", "|\n\nLocal composite actions"},
		},
		{
			// Where the conflation lived: the table drops its Parent column when ANY scan is
			// unattributed, and the caveat once borrowed that same flag - discarding a local
			// action one scan had definitely found. The two questions are separate, so the
			// section must state both the known path and the incomplete knowledge.
			name: "verify when one scan found a local action and another was not attributed then both are stated",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{
				{CuratedActions: &formats.CuratedActions{
					Attributed:            true,
					Actions:               []formats.CuratedAction{{Action: "actions/checkout", Ref: "v4", Status: "Approved"}},
					LocalCompositeActions: []formats.LocalCompositeAction{{Path: "./.github/actions/setup"}},
				}},
				{CuratedActions: actions(false, formats.CuratedAction{Action: "actions/cache", Ref: "v4", Status: "Approved"})},
			}}},
			wantContains: []string{
				"| Action | Ref | Status | Notes |", // the Parent column still drops, as before
				"./.github/actions/setup",
				"there may be others it could not see",
			},
		},
		{
			// Two jobs' summary files merged into one section. The table already drops the Parent
			// column on mixed data; the caveat has to name every local action across them, since
			// dropping one would understate the gap in exactly the case with most to state.
			name: "verify when several scans declare local actions then every one is named",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{
				{CuratedActions: &formats.CuratedActions{
					Attributed:            true,
					Actions:               []formats.CuratedAction{{Action: "actions/checkout", Ref: "v4", Status: "Approved"}},
					LocalCompositeActions: []formats.LocalCompositeAction{{Path: "./.github/actions/setup"}},
				}},
				{CuratedActions: &formats.CuratedActions{
					Attributed:            true,
					Actions:               []formats.CuratedAction{{Action: "actions/cache", Ref: "v4", Status: "Approved"}},
					LocalCompositeActions: []formats.LocalCompositeAction{{Path: "./.github/actions/setup"}, {Path: "./.github/actions/teardown", DeclaredBy: "some-org/wrapper@v1"}},
				}},
			}}},
			wantContains: []string{"./.github/actions/setup", "./.github/actions/teardown"},
		},
		{
			// Same contract as RenderReportTable's console output: the job summary is rendered by
			// GitHub, so an unescaped "|" or newline reshapes the table a reviewer actually reads.
			name: "verify when cells hold a pipe or a newline then the row keeps the header's cell count",
			data: []formats.ResultsSummary{{Scans: []formats.ScanSummary{{
				Target: ".github/workflows/ci.yml",
				CuratedActions: actions(true,
					formats.CuratedAction{Action: "some-org/some-action", Ref: "feature|v2", Parent: "org/wrap|per@v1", Status: "Rejected", Notes: "blocked:\nCVE-2024-0001"},
				),
			}}}},
			wantContains: []string{`| some-org/some-action | feature\|v2 | org/wrap\|per@v1 | Rejected | blocked:<br>CVE-2024-0001 |`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			markdown, err := GenerateActionsCurationSectionMarkdown(tt.data)
			require.NoError(t, err)

			if tt.wantEmpty {
				assert.Equal(t, "", markdown)
				return
			}
			for _, want := range tt.wantContains {
				assert.Contains(t, markdown, want)
			}
			for _, notWant := range tt.wantNotContains {
				assert.NotContains(t, markdown, notWant)
			}
		})
	}
}

func TestSecurityJobSummary_GenerateMarkdownFromFiles(t *testing.T) {
	const curationHeading, actionsHeading = "Curation Audit", "GitHub Actions Curation"
	approvedCheckout := []formats.CuratedAction{{Action: "actions/checkout", Ref: "v4", Status: "Approved"}}
	tests := []struct {
		name string
		// packages is a curation-audit run's summary; nil when curation-audit did not run.
		packages *formats.ResultsSummary
		// actions is a curate-gh-actions run's report; nil when it did not run.
		actions *formats.CuratedActions
		// wantSections are the section headings in the order they must appear; a known heading
		// ("Curation Audit", "GitHub Actions Curation") not listed must be absent.
		wantSections []string
		wantContains []string
	}{
		{
			name:         "verify when both commands ran then the curation-audit section comes before the actions section",
			packages:     &formats.ResultsSummary{Scans: []formats.ScanSummary{{Target: "npm-project", CuratedPackages: &formats.CuratedPackages{PackageCount: 1}}}},
			actions:      &formats.CuratedActions{Attributed: true, Actions: approvedCheckout},
			wantSections: []string{curationHeading, actionsHeading},
			wantContains: []string{"actions/checkout"},
		},
		{
			// curate-gh-actions shares the "security" command-summary manager with curation-audit and
			// appends to the same markdown. A run where only curation-audit executed must therefore be
			// byte-for-byte what it was before the actions section existed - no stray heading, no
			// trailing newline, nothing.
			name:         "verify when only curation-audit ran then its section is exactly what it was without the actions section",
			packages:     &formats.ResultsSummary{Scans: []formats.ScanSummary{{Target: "npm-project", CuratedPackages: &formats.CuratedPackages{PackageCount: 3}}}},
			wantSections: []string{curationHeading},
		},
		{
			// The caveat is rendered from the summary file, so what the command knew about local
			// composite actions has to survive the round trip rather than stopping at the console.
			name: "verify when the actions report declared a local composite action then the caveat survives the summary file",
			actions: &formats.CuratedActions{
				Attributed:            true,
				Actions:               approvedCheckout,
				LocalCompositeActions: []formats.LocalCompositeAction{{Path: "./.github/actions/setup"}},
			},
			wantSections: []string{actionsHeading},
			wantContains: []string{"Not covered", "./.github/actions/setup"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var files []string
			var curationFile string
			if tt.packages != nil {
				curationFile = writeSummaryDataFile(t, NewCurationSummary(*tt.packages))
				files = append(files, curationFile)
			}
			if tt.actions != nil {
				files = append(files, writeSummaryDataFile(t, NewCurationActionsSummary(*tt.actions)))
			}

			js := &SecurityJobSummary{}
			markdown, err := js.GenerateMarkdownFromFiles(files)
			require.NoError(t, err)

			last := -1
			for _, heading := range tt.wantSections {
				at := strings.Index(markdown, heading)
				assert.Greater(t, at, last, "heading %q must appear after the previous one", heading)
				last = at
			}
			for _, heading := range []string{curationHeading, actionsHeading} {
				if !slices.Contains(tt.wantSections, heading) {
					assert.NotContains(t, markdown, heading)
				}
			}
			for _, want := range tt.wantContains {
				assert.Contains(t, markdown, want)
			}
			if tt.actions == nil {
				// The curation section rendered on its own, which is what the pipeline produced before.
				curationData, _, err := loadContent([]string{curationFile}, utils.Curation)
				require.NoError(t, err)
				expected, err := GenerateSecuritySectionMarkdown(curationData)
				require.NoError(t, err)
				assert.Equal(t, expected, markdown, "a curation-audit-only run must gain nothing from the actions section")
			}
		})
	}
}
