package githubactions

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewActionReportRow(t *testing.T) {
	tests := []struct {
		name string
		ref  ActionRef
		want ActionReportRow
	}{
		{
			// init@v3 and analyze@v3 collapse to one cache entry, so neither invocation may be lost.
			name: "verify when the action has several subpaths then every one is listed in the action cell",
			ref:  ActionRef{Owner: "github", Repo: "codeql-action", Ref: "v3", Subpaths: []string{"init", "analyze"}},
			want: ActionReportRow{Action: "github/codeql-action (init, analyze)", Ref: "v3", Status: "Approved"},
		},
		{
			// An unpaired logged commit has no ref; its SHA is what the reader and the gate's message need.
			name: "verify when the ref is an unpaired logged commit then the ref cell names its SHA",
			ref:  ActionRef{Owner: "actions", Repo: "checkout", RunnerSHA: shaV4, Verification: VerifyLoggedSHA},
			want: ActionReportRow{Action: "actions/checkout", Ref: shaV4, Status: "Approved"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, NewActionReportRow(tt.ref, ActionCurationResult{Status: ActionApproved}))
		})
	}
}

// tableCells splits a rendered report line into its cells. An escaped pipe sits inside a cell, never
// between spaces, so splitting on " | " keeps it in its cell.
func tableCells(line string) []string {
	return strings.Split(strings.TrimSuffix(strings.TrimPrefix(line, "| "), " |"), " | ")
}

func TestRenderReportTable(t *testing.T) {
	tests := []struct {
		name       string
		rows       []ActionReportRow
		withParent bool
		wantHeader []string   // the header row's cells
		wantCells  [][]string // each data row's cells, one line per row
	}{
		{
			name: "verify when the run was attributed then every row carries a Parent cell",
			rows: []ActionReportRow{
				{Action: "actions/checkout", Ref: "v4", Status: "Approved"},
				{Action: "some-org/transitive-action", Ref: "v1", Parent: "github/codeql-action@v3", Status: "Rejected", Notes: "policy failure"},
			},
			withParent: true,
			wantHeader: []string{"Action", "Ref", "Parent", "Status", "Notes"},
			wantCells: [][]string{
				{"actions/checkout", "v4", "", "Approved", ""},
				{"some-org/transitive-action", "v1", "github/codeql-action@v3", "Rejected", "policy failure"},
			},
		},
		{
			// Present-and-blank would read as "nothing pulled in transitively".
			name: "verify when the run was structure-only then the Parent column is omitted",
			rows: []ActionReportRow{
				{Action: "actions/checkout", Ref: "v4", Status: "Approved"},
				{Action: "some-org/some-action", Ref: "v1", Status: "Rejected", Notes: "policy failure"},
			},
			wantHeader: []string{"Action", "Ref", "Status", "Notes"},
			wantCells: [][]string{
				{"actions/checkout", "v4", "Approved", ""},
				{"some-org/some-action", "v1", "Rejected", "policy failure"},
			},
		},
		{
			// "|" is legal in a git refname and Notes comes from the decision service: unescaped, either would
			// add a column or split the row, so the table would report a status against the wrong action. The
			// newline becomes a space, not "<br>": this table is printed to the job log.
			name:       "verify when cells hold a pipe or a newline then the row keeps its cells on one line",
			rows:       []ActionReportRow{{Action: "some-org/some-action", Ref: "feature|v2", Parent: "org/wrap|per@v1", Status: "Rejected", Notes: "blocked:\nCVE-2024-0001"}},
			withParent: true,
			wantHeader: []string{"Action", "Ref", "Parent", "Status", "Notes"},
			wantCells:  [][]string{{"some-org/some-action", `feature\|v2`, `org/wrap\|per@v1`, "Rejected", "blocked: CVE-2024-0001"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RenderReportTable(tt.rows, tt.withParent)

			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			if !assert.Len(t, lines, 2+len(tt.rows), "RenderReportTable() = %q: header, separator, one line per row", got) {
				return
			}
			assert.Equal(t, tt.wantHeader, tableCells(lines[0]), "RenderReportTable() header")
			var gotCells [][]string
			for _, line := range lines[2:] {
				gotCells = append(gotCells, tableCells(line))
			}
			assert.Equal(t, tt.wantCells, gotCells, "RenderReportTable() rows")
		})
	}
}

func TestNotApproved(t *testing.T) {
	tests := []struct {
		name string
		rows []ActionReportRow
		want []string // the Ref of each row that must not clear the gate
	}{
		{name: "verify when every action is approved then none is withheld", rows: []ActionReportRow{{Ref: "v4", Status: "Approved"}}},
		{name: "verify when no action was decided then none is withheld", rows: nil},
		{
			name: "verify when an action is rejected then it is withheld",
			rows: []ActionReportRow{{Ref: "v4", Status: "Approved"}, {Ref: "v1", Status: "Rejected"}},
			want: []string{"v1"},
		},
		{
			// A deny-list would let these through while rendering an empty or unfamiliar cell: the
			// zero value of a result returned without a status, and a status a later decider adds.
			name: "verify when a status is blank or unrecognized then it is withheld",
			rows: []ActionReportRow{{Ref: "v4", Status: "Approved"}, {Ref: "v9"}, {Ref: "v2", Status: "NeedsReview"}},
			want: []string{"v9", "v2"},
		},
		{
			// Case matters: only the exact constant approves.
			name: "verify when a status differs only in case then it is withheld",
			rows: []ActionReportRow{{Ref: "v4", Status: "approved"}},
			want: []string{"v4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, row := range NotApproved(tt.rows) {
				got = append(got, row.Ref)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}
