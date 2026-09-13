package githubactions

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jfrog/jfrog-client-go/utils/log"
)

// WorkflowUse is one `uses:` value parsed out of a workflow (or composite action) YAML file.
type WorkflowUse struct {
	Owner   string
	Repo    string
	Subpath string // "" handle mono repo github actions, e.g. github/codeql-action/analyze@v3
	Ref     string
	Raw     string // the original "uses:" string, for diagnostics
}

type rawWorkflow struct {
	Jobs map[string]rawJob `yaml:"jobs"`
}

type rawJob struct {
	Steps []rawStep `yaml:"steps"`
}

type rawStep struct {
	Uses string `yaml:"uses"`
}

// parseUsesString parses a single `uses:` value into owner/repo/ref, plus an optional subpath
// (empty for most actions - only present for monorepo-style actions like
// github/codeql-action/analyze@v3). Returns false for shapes that don't resolve to at least an
// owner/repo/ref triple
func parseUsesString(raw string) (WorkflowUse, bool) {
	if raw == "" || strings.HasPrefix(raw, "./") || strings.HasPrefix(raw, "docker://") {
		return WorkflowUse{}, false
	}
	atIdx := strings.LastIndex(raw, "@")
	if atIdx < 0 || atIdx == len(raw)-1 {
		return WorkflowUse{}, false
	}
	path, ref := raw[:atIdx], raw[atIdx+1:]
	segments := strings.Split(path, "/")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return WorkflowUse{}, false
	}
	subpath := ""
	if len(segments) > 2 {
		subpath = strings.Join(segments[2:], "/")
	}
	return WorkflowUse{Owner: segments[0], Repo: segments[1], Subpath: subpath, Ref: ref, Raw: raw}, true
}

// ErrJobUnknown reports that the job being curated could not be identified in this workflow
// file - either no job id was given, or the file does not declare the one that was. It is not a
// failure of the run: callers treat it as "cannot attribute" and curate the cache as-is.
var ErrJobUnknown = errors.New("cannot identify the job being curated in the workflow file")

// ParseWorkflowUses parses the step-level `uses:` values of ONE job in a workflow YAML file.
// Local actions (uses: ./path) and Docker-URI actions (uses: docker://...) are skipped.
//
// jobID must name a job the file declares; otherwise it returns ErrJobUnknown and parses
// nothing. There is deliberately no fallback to the file's other jobs: each ran on its own
// runner with its own cache, so attributing from them would label an entry with a parent that
// never pulled it in. Attribution therefore needs both a file and a job id - no constraint on a
// runner, where GITHUB_JOB is always set.
func ParseWorkflowUses(workflowPath, jobID string) ([]WorkflowUse, error) {
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		return nil, fmt.Errorf("reading workflow file %q: %w", workflowPath, err)
	}
	var wf rawWorkflow
	if err = yaml.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parsing workflow file %q: %w", workflowPath, err)
	}
	if jobID == "" {
		return nil, fmt.Errorf("%w: no job id given for %q", ErrJobUnknown, workflowPath)
	}
	job, declared := wf.Jobs[jobID]
	if !declared {
		return nil, fmt.Errorf("%w: %q is not among %v in %q", ErrJobUnknown, jobID, slices.Sorted(maps.Keys(wf.Jobs)), workflowPath)
	}
	// One job's steps: a slice, so the order is the file's, with no map iteration to sort away.
	var uses []WorkflowUse
	for _, step := range job.Steps {
		if parsed, ok := parseUsesString(step.Uses); ok {
			uses = append(uses, parsed)
		}
	}
	return uses, nil
}

type rawActionFile struct {
	Runs rawActionRuns `yaml:"runs"`
}

type rawActionRuns struct {
	Using string    `yaml:"using"`
	Steps []rawStep `yaml:"steps"`
}

// parseCompositeActionUses reads <actionPath>/action.yml (or action.yaml) and, if it's a
// composite action, returns every owner/repo/ref its own steps reference - one hop outward from
// actionPath. CrossReference calls this repeatedly, once per action per round, to walk
// arbitrarily many hops; this function itself only ever looks at the one action.yml it's given.
//
// There is no error return because there is no failure: absent metadata, metadata this parser
// cannot read, and a non-composite action all mean the same thing here - nothing to attribute
// from - and none of them may fail the run. The result is always "the references found", which
// is legitimately none.
func parseCompositeActionUses(actionPath string) []WorkflowUse {
	for _, name := range []string{"action.yml", "action.yaml"} {
		data, err := os.ReadFile(filepath.Join(actionPath, name))
		if err != nil {
			continue
		}
		var af rawActionFile
		if err := yaml.Unmarshal(data, &af); err != nil {
			// The runner already accepted this file, so a parse failure here is a divergence
			// between its YAML reader and ours, not a broken action. Nothing is attributed from
			// it - the entries it pulled in stay unattributed and are still curated - but the
			// reason has to be greppable, or the missing Parent column looks like a design choice.
			log.Debug(fmt.Sprintf("github-actions curation: cannot parse %q - no transitive references attributed from it: %v", filepath.Join(actionPath, name), err))
			return nil
		}
		if af.Runs.Using != "composite" {
			return nil
		}
		var uses []WorkflowUse
		for _, step := range af.Runs.Steps {
			if parsed, ok := parseUsesString(step.Uses); ok {
				uses = append(uses, parsed)
			}
		}
		return uses
	}
	return nil
}

// CrossReference enriches discovered entries with Subpaths and best-effort Parent metadata,
// and returns the enriched slice.
//
// Attribution is purely additive: it only ever adds metadata, never removes an entry. One it
// cannot place keeps an empty Parent and is still curated - an action the runner resolved will
// execute whether or not this code can explain why it is there.
//
// A directly-used entry takes its Subpaths from the job's own uses: lines. Every other entry is
// attributed by walking outward one level at a time, reading the action.yml of each composite
// action resolved at the current depth: a step referencing an unresolved entry makes that
// entry's Parent the composite action's "<owner>/<repo>@<ref>", and that entry a source for the
// next level.
//
// The walk has no fixed depth limit - it stops when the frontier runs dry, within len(discovered)
// rounds, since every round attributes at least one previously-unattributed entry. A cycle
// terminates for the same reason: each action.yml is read at most once.
//
// KNOWN LIMITATION: an action pulling others in via a run: step rather than its own uses:, and
// actions used by a called reusable workflow (jobs.<id>.uses:), are never attributed - Parent
// stays empty, never guessed. Unattributed is not unreported; those entries are still curated.
func CrossReference(discovered []ActionRef, used []WorkflowUse) []ActionRef {
	byKey := make(map[string]int, len(discovered))
	for i := range discovered {
		byKey[refKey(discovered[i].Owner, discovered[i].Repo, discovered[i].Ref)] = i
	}

	subpathsByKey := collectSubpaths(used)

	// attributed marks every key that already has its Parent/Subpaths resolved (directly, or
	// transitively by an earlier/shallower round) - a source for the next level's walk, and a
	// guard against a deeper round overwriting an already-settled (shallower) attribution.
	attributed := map[string]bool{}
	frontier := make([]string, 0, len(used))
	for _, u := range used {
		key := refKey(u.Owner, u.Repo, u.Ref)
		if attributed[key] {
			continue
		}
		attributed[key] = true
		if idx, ok := byKey[key]; ok {
			discovered[idx].Subpaths = subpathsByKey[key]
		}
		frontier = append(frontier, key)
	}

	// A second, independent bound: a regression in the dedup above would hit a hard stop rather
	// than spin. It is not a limit on legitimate nesting depth.
	maxRounds := len(discovered) + 1
	// visited is keyed by "parentKey\x00subpath" (subpath "" for the no-subpath/root case),
	// since a monorepo action invoked via more than one subpath (e.g. codeql-action's init and
	// analyze) has a separate action.yml per subpath, each needing its own scan.
	visited := map[string]bool{}
	for depth := 0; depth < maxRounds && len(frontier) > 0; depth++ {
		var nextFrontier []string
		for _, parentKey := range frontier {
			parentIdx, ok := byKey[parentKey]
			if !ok {
				continue
			}
			parentIdentity := fmt.Sprintf("%s/%s@%s", discovered[parentIdx].Owner, discovered[parentIdx].Repo, discovered[parentIdx].Ref)

			// For owner/repo/subpath@ref, the action's own metadata lives at
			// <cache>/<owner>/<repo>/<ref>/<subpath>/action.yml, not at the cache root - the root
			// is only correct when the action was never referenced via a subpath.
			subpaths := discovered[parentIdx].Subpaths
			if len(subpaths) == 0 {
				subpaths = []string{""}
			}
			for _, subpath := range subpaths {
				visitKey := parentKey + "\x00" + subpath
				if visited[visitKey] {
					continue
				}
				visited[visitKey] = true

				metadataDir := discovered[parentIdx].Path
				if subpath != "" {
					metadataDir = filepath.Join(metadataDir, subpath)
				}
				compositeUses := parseCompositeActionUses(metadataDir)
				if len(compositeUses) == 0 {
					continue
				}
				childSubpaths := collectSubpaths(compositeUses)
				for _, cu := range compositeUses {
					childKey := refKey(cu.Owner, cu.Repo, cu.Ref)
					if attributed[childKey] {
						continue
					}
					childIdx, ok := byKey[childKey]
					if !ok {
						continue
					}
					discovered[childIdx].Parent = parentIdentity
					discovered[childIdx].Subpaths = childSubpaths[childKey]
					attributed[childKey] = true
					nextFrontier = append(nextFrontier, childKey)
				}
			}
		}
		frontier = nextFrontier
	}
	return discovered
}

// collectSubpaths deduplicates the Subpath of every use by its owner/repo/ref key, preserving
// first-seen order. Uses with an empty Subpath contribute nothing (most actions have none).
func collectSubpaths(uses []WorkflowUse) map[string][]string {
	seen := map[string]map[string]bool{}
	result := map[string][]string{}
	for _, u := range uses {
		if u.Subpath == "" {
			continue
		}
		key := refKey(u.Owner, u.Repo, u.Ref)
		if seen[key] == nil {
			seen[key] = map[string]bool{}
		}
		if seen[key][u.Subpath] {
			continue
		}
		seen[key][u.Subpath] = true
		result[key] = append(result[key], u.Subpath)
	}
	return result
}

func refKey(owner, repo, ref string) string {
	return owner + "/" + repo + "@" + ref
}
