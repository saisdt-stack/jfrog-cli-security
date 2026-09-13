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
	return ActionReportRow{
		Action: action,
		Ref:    ref.Ref,
		Parent: ref.Parent,
		Status: string(result.Status),
		Notes:  result.Notes,
	}
}

// RenderMarkdownTable renders rows as a GitHub-flavored markdown table. withParent controls
// whether the Parent column appears at all.
func RenderMarkdownTable(rows []ActionReportRow, withParent bool) string {
	var sb strings.Builder
	if withParent {
		sb.WriteString("| Action | Ref | Parent | Status | Notes |\n")
		sb.WriteString("|--------|-----|--------|--------|-------|\n")
	} else {
		sb.WriteString("| Action | Ref | Status | Notes |\n")
		sb.WriteString("|--------|-----|--------|-------|\n")
	}
	for _, row := range rows {
		sb.WriteString("| ")
		sb.WriteString(formats.EscapeMarkdownTableCell(row.Action))
		sb.WriteString(" | ")
		sb.WriteString(formats.EscapeMarkdownTableCell(row.Ref))
		if withParent {
			sb.WriteString(" | ")
			sb.WriteString(formats.EscapeMarkdownTableCell(row.Parent))
		}
		sb.WriteString(" | ")
		sb.WriteString(formats.EscapeMarkdownTableCell(row.Status))
		sb.WriteString(" | ")
		sb.WriteString(formats.EscapeMarkdownTableCell(row.Notes))
		sb.WriteString(" |\n")
	}
	return sb.String()
}

// AnyRejected reports whether any row's Status is ActionRejected, for the command's exit-code decision.
func AnyRejected(rows []ActionReportRow) bool {
	for _, row := range rows {
		if row.Status == string(ActionRejected) {
			return true
		}
	}
	return false
}
