package githubactions

import (
	"strings"

	"github.com/jfrog/jfrog-cli-security/utils/formats"
)

// ActionReportRow is one row of the curation report, already resolved from an ActionRef and
// its ActionCurationResult.
type ActionReportRow struct {
	Action string // "owner/repo", plus " (subpath[, subpath...])" when invoked via subpaths
	Ref    string // verbatim from the cache directory name, uninterpreted
	Parent string // "" when directly referenced, or when attribution could not place it
	Status string
	Notes  string
}

// NewActionReportRow builds one report row. A monorepo action invoked via several subpaths
// (codeql-action's init@v3 and analyze@v3) shares one cache entry and one decision, so it is
// one row - every subpath used is listed so neither invocation is silently lost.
func NewActionReportRow(ref ActionRef, result ActionCurationResult) ActionReportRow {
	action := ref.Owner + "/" + ref.Repo
	if len(ref.Subpaths) > 0 {
		action += " (" + strings.Join(ref.Subpaths, ", ") + ")"
	}
	// An unpaired logged commit has no ref; its SHA is what the reader and the gate's message need.
	shown := ref.Ref
	if ref.Unpaired() {
		shown = ref.RunnerSHA
	}
	return ActionReportRow{
		Action: action,
		Ref:    shown,
		Parent: ref.Parent,
		Status: string(result.Status),
		Notes:  result.Notes,
	}
}

// RenderReportTable renders rows as the console report's table. withParent controls whether the
// Parent column appears at all.
//
// Pipe-delimited like the job summary's table, so the two read alike and either can be pasted
// where the other is expected - but the cells are escaped for a terminal, because this one is
// printed to the job log and read there. The job summary renders its own table from the recorded
// summary files; the shared piece is the cell escaping, not the table.
func RenderReportTable(rows []ActionReportRow, withParent bool) string {
	headers := []string{"Action", "Ref"}
	if withParent {
		headers = append(headers, "Parent")
	}
	headers = append(headers, "Status", "Notes")

	var sb strings.Builder
	sb.WriteString("| " + strings.Join(headers, " | ") + " |\n|")
	for _, header := range headers {
		sb.WriteString(strings.Repeat("-", len(header)+2) + "|")
	}
	sb.WriteString("\n")
	for _, row := range rows {
		cells := []string{row.Action, row.Ref}
		if withParent {
			cells = append(cells, row.Parent)
		}
		cells = append(cells, row.Status, row.Notes)
		for i := range cells {
			cells[i] = formats.EscapeTerminalTableCell(cells[i])
		}
		sb.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return sb.String()
}

// NotApproved returns every row whose Status is not exactly ActionApproved, for the command's
// exit-code decision.
//
// An allow-list, not a check for Rejected: an unknown or zero-value status must fail the gate too.
func NotApproved(rows []ActionReportRow) []ActionReportRow {
	var notApproved []ActionReportRow
	for _, row := range rows {
		if row.Status != string(ActionApproved) {
			notApproved = append(notApproved, row)
		}
	}
	return notApproved
}
