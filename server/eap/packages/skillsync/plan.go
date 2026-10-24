// Desired-state planning for top-level `eap sync` (TASK-018): the project's
// .eap/workspace.yaml declares the skills each Agent target must have, and
// the planner diffs that desired state against the lockfile and the server
// catalog. Planning is pure — no filesystem writes — so `--dry-run` is exact
// and ApplyPlan converges only EAP-managed files.
package skillsync

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"enterprise-agent-platform/packages/errs"
)

// CatalogEntry is one row of the workspace skill catalog.
type CatalogEntry struct {
	Name    string
	Version string
	Status  string // released | deprecated | ...
}

// CatalogSource is the optional server surface the planner needs to resolve
// unspecified versions to the latest released one.
type CatalogSource interface {
	Catalog(ctx context.Context) ([]CatalogEntry, error)
}

// DesiredSkill is one skill the workspace wants installed. Version "" means
// "latest released".
type DesiredSkill struct {
	Name    string
	Version string
}

// DesiredState is the authoritative desired state for the target clients.
type DesiredState struct {
	Skills  []DesiredSkill
	Clients []string
}

// PlanAction kinds.
const (
	ActionInstall   = "install"
	ActionUpgrade   = "upgrade"
	ActionUnchanged = "unchanged"
	ActionRemove    = "remove"
)

// PlanAction is one convergence step for one (client, skill) pair.
type PlanAction struct {
	Client string `json:"client"`
	Name   string `json:"name"`
	Action string `json:"action"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
}

// Plan diffs the desired state against the lockfile and catalog. A lockfile
// entry for a client that is absent from the desired skills converges to
// remove (desired state is authoritative); skills with a specified version
// that the catalog no longer offers as released fail closed instead of being
// silently removed.
func Plan(ctx context.Context, c CatalogSource, o Options, desired DesiredState) ([]PlanAction, error) {
	lock, err := LoadLockfile(o.LockfilePath)
	if err != nil {
		return nil, err
	}
	catalog, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}

	var plan []PlanAction
	for _, client := range desired.Clients {
		want := map[string]string{}
		for _, d := range desired.Skills {
			version := d.Version
			if version == "" {
				latest, ok := latestReleased(catalog, d.Name)
				if !ok {
					return nil, errs.New(errs.NotFound,
						fmt.Sprintf("no released version of skill %q — desired state cannot be converged", d.Name))
				}
				version = latest
			} else if status, ok := catalogStatus(catalog, d.Name, version); !ok || status != "released" {
				return nil, errs.New(errs.InvalidArgument,
					fmt.Sprintf("skill %q@%s is not a released version in this Workspace — desired state cannot converge", d.Name, version))
			}
			want[d.Name] = version
		}
		for name, version := range want {
			current := lockVersion(lock, client, name)
			switch {
			case current == "":
				plan = append(plan, PlanAction{Client: client, Name: name, Action: ActionInstall, To: version})
			case current == version:
				plan = append(plan, PlanAction{Client: client, Name: name, Action: ActionUnchanged, To: version})
			default:
				plan = append(plan, PlanAction{Client: client, Name: name, Action: ActionUpgrade, From: current, To: version})
			}
		}
		for _, e := range lock.Entries {
			if e.Client != client {
				continue
			}
			if _, ok := want[e.Name]; !ok {
				plan = append(plan, PlanAction{Client: client, Name: e.Name, Action: ActionRemove, From: e.Version})
			}
		}
	}
	sortPlan(plan)
	return plan, nil
}

// ApplyPlan executes a plan deterministically: removals first, then
// installs/upgrades. Unchanged actions are skipped. Any failure aborts with
// the engine's own prior-state restoration for the failing entry.
func ApplyPlan(ctx context.Context, c Client, o Options, plan []PlanAction) ([]SyncResult, error) {
	ordered := make([]PlanAction, 0, len(plan))
	for _, a := range plan {
		if a.Client != o.Client {
			continue
		}
		if a.Action == ActionRemove {
			ordered = append([]PlanAction{a}, ordered...)
			continue
		}
		ordered = append(ordered, a)
	}
	var results []SyncResult
	for _, a := range ordered {
		switch a.Action {
		case ActionRemove:
			r, err := Sync(ctx, c, o, a.Name, "", true)
			if err != nil {
				return results, err
			}
			results = append(results, r)
		case ActionInstall, ActionUpgrade:
			r, err := Sync(ctx, c, o, a.Name, a.To, false)
			if err != nil {
				return results, err
			}
			results = append(results, r)
		}
	}
	return results, nil
}

func lockVersion(lock Lockfile, client, name string) string {
	for _, e := range lock.Entries {
		if e.Client == client && e.Name == name {
			return e.Version
		}
	}
	return ""
}

// catalogStatus returns the registry status of one catalog row.
func catalogStatus(catalog []CatalogEntry, name, version string) (string, bool) {
	for _, row := range catalog {
		if row.Name == name && row.Version == version {
			return row.Status, true
		}
	}
	return "", false
}

// latestReleased picks the highest released version for name. Version
// ordering is numeric-segment aware so 0.10.0 sorts above 0.9.0.
func latestReleased(catalog []CatalogEntry, name string) (string, bool) {
	best := ""
	found := false
	for _, row := range catalog {
		if row.Name != name || row.Status != "released" {
			continue
		}
		if !found || CompareVersions(row.Version, best) > 0 {
			best, found = row.Version, true
		}
	}
	return best, found
}

// CompareVersions orders dotted numeric version segments; 0.10.0 > 0.9.0.
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av > bv {
				return 1
			}
			return -1
		}
	}
	return strings.Compare(a, b)
}

// sortPlan makes plan output stable: by client, then remove-first, then name.
func sortPlan(plan []PlanAction) {
	sort.SliceStable(plan, func(i, j int) bool {
		if plan[i].Client != plan[j].Client {
			return plan[i].Client < plan[j].Client
		}
		ri, rj := plan[i].Action == ActionRemove, plan[j].Action == ActionRemove
		if ri != rj {
			return ri
		}
		return plan[i].Name < plan[j].Name
	})
}
