package cli

// Top-level `eap sync` (TASK-018): converges the desired skill state declared
// in .eap/workspace.yaml onto every selected Agent target. The declaration is
// data, never code; planning is exact before apply, `--dry-run` performs no
// writes, and convergence touches only EAP-managed files.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"enterprise-agent-platform/packages/skillsync"
)

// desiredState is the project's declared skill state.
type desiredState struct {
	Clients []string
	Skills  []skillsync.DesiredSkill
}

// loadDesiredState reads .eap/workspace.yaml. Missing skills/clients keys
// default to no skills and the harness+codex qualification pair.
func loadDesiredState(workdir string) (desiredState, error) {
	v := viper.New()
	v.SetConfigFile(filepath.Join(workdir, projectConfig))
	if err := v.ReadInConfig(); err != nil {
		// No project config: still a valid empty desired state.
		return desiredState{Clients: []string{"harness", "codex"}}, nil
	}
	out := desiredState{
		Clients: v.GetStringSlice("clients"),
	}
	if len(out.Clients) == 0 {
		out.Clients = []string{"harness", "codex"}
	}
	for _, raw := range v.GetStringSlice("skills") {
		name, version := splitNameVersion(strings.TrimSpace(raw))
		if name == "" {
			return out, &usageError{msg: fmt.Sprintf("invalid desired skill entry %q — expected name or name@version", raw)}
		}
		out.Skills = append(out.Skills, skillsync.DesiredSkill{Name: name, Version: version})
	}
	return out, nil
}

func newSyncCmd(o *Options) *cobra.Command {
	var dryRun bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "Converge .eap/workspace.yaml desired skills onto every target client (verified, atomic, idempotent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			workdir := o.Workdir
			if workdir == "" {
				workdir = "."
			}
			desired, err := loadDesiredState(workdir)
			if err != nil {
				return err
			}
			for _, client := range desired.Clients {
				if _, ok := lookupSkillAdapter(client); !ok {
					return &usageError{msg: fmt.Sprintf("unsupported client %q (supported: harness, codex, claude-code, pi, project)", client)}
				}
			}
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			keys, err := o.PackageKeys(cmd.Context(), o.Server)
			if err != nil {
				return err
			}
			lockPath := filepath.Join(workdir, "eap.lock")
			source := &catalogSource{server: o.Server, bearer: bearer, workspace: o.Workspace}
			plan, err := skillsync.Plan(cmd.Context(), source, skillsync.Options{LockfilePath: lockPath},
				skillsync.DesiredState{Skills: desired.Skills, Clients: desired.Clients})
			if err != nil {
				return err
			}
			if o.JSON {
				printJSON(o, map[string]any{"dry_run": dryRun, "plan": plan})
			} else {
				for _, a := range plan {
					switch a.Action {
					case skillsync.ActionUnchanged:
						printHuman(o, "unchanged\t%s\t%s@%s", a.Client, a.Name, a.To)
					case skillsync.ActionRemove:
						printHuman(o, "remove\t%s\t%s (当前 %s)", a.Client, a.Name, a.From)
					case skillsync.ActionUpgrade:
						printHuman(o, "upgrade\t%s\t%s %s → %s", a.Client, a.Name, a.From, a.To)
					default:
						printHuman(o, "install\t%s\t%s@%s", a.Client, a.Name, a.To)
					}
				}
			}
			if dryRun {
				return nil
			}
			for _, client := range desired.Clients {
				clientRoot := AgentSkillRoot(workdir, client)
				results, err := skillsync.ApplyPlan(cmd.Context(), &gatewayDownloader{server: o.Server, bearer: bearer, workspace: o.Workspace},
					syncOptions(clientRoot, lockPath, client, keys), plan)
				if err != nil {
					return err
				}
				for _, r := range results {
					printHuman(o, "已同步 %s@%s → %s（lockfile %s）", r.Name, r.Version, clientRoot, lockPath)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&dryRun, "dry-run", false, "show the convergence plan without writing anything")
	return c
}
