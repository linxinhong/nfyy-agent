package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"enterprise-agent-platform/packages/skillreg"
	"enterprise-agent-platform/packages/skillsync"
)

// newSkillCmd implements `eap skill` (TASK-018): publish is a server-side
// flow; sync installs verified releases into client roots.
func newSkillCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "skill", Short: "Skill development and managed installation"}
	var client, root, lockfile string
	c.PersistentFlags().StringVar(&client, "client", "harness", "target client (harness|codex|claude-code|pi|project)")
	c.PersistentFlags().StringVar(&root, "root", "", "managed install root (overrides the selected Agent adapter)")
	c.PersistentFlags().StringVar(&lockfile, "lockfile", "", "lockfile path (default <workdir>/eap.lock)")
	resolve := func() (clientRoot, lockPath string) {
		base := o.Workdir
		if base == "" {
			base = "."
		}
		if root == "" {
			clientRoot = AgentSkillRoot(base, client)
		} else {
			clientRoot = root
		}
		if lockfile == "" {
			lockPath = filepath.Join(base, "eap.lock")
		} else {
			lockPath = lockfile
		}
		return
	}
	init := &cobra.Command{
		Use: "init <name>", Short: "Create a standard Skill project",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := filepath.Join(o.Workdir, args[0])
			if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+args[0]+"\n\nDescribe when and how to use this Skill.\n"), 0o644); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "skill.yaml"), []byte("name: "+args[0]+"\nversion: 0.1.0\nrisk: R1\n"), 0o644); err != nil {
				return err
			}
			printHuman(o, "已创建 %s", dir)
			return nil
		},
	}
	validate := &cobra.Command{Use: "validate <dir>", Short: "Validate and secret-scan a Skill checkout", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		files, err := skillFiles(args[0])
		if err != nil {
			return err
		}
		printHuman(o, "Skill 有效（%d 个文件）", len(files))
		return nil
	}}
	pack := &cobra.Command{Use: "pack <dir> <output>", Short: "Create a deterministic Skill archive", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		files, err := skillFiles(args[0])
		if err != nil {
			return err
		}
		archive, digest, err := skillreg.BuildArchive(files)
		if err != nil {
			return err
		}
		if err := os.WriteFile(args[1], archive, 0o644); err != nil {
			return err
		}
		printHuman(o, "已打包 %s（sha256:%s）", args[1], digest)
		return nil
	}}
	publish := &cobra.Command{Use: "publish <name> <version> <dir>", Short: "Submit a Skill version for review", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		bearer, err := o.authBearer(cmd.Context())
		if err != nil {
			return err
		}
		files, err := skillFiles(args[2])
		if err != nil {
			return err
		}
		status, body, err := jsonRequest(cmd.Context(), o.Server, "/enterprise/v1/skills/"+args[0]+"/versions", httpPost, bearer, map[string]any{"version": args[1], "files": files})
		if err != nil {
			return err
		}
		if status != 201 {
			return fmt.Errorf("skill publish failed (%d): %s", status, redact(string(body)))
		}
		printHuman(o, "已提交 %s@%s，等待审核", args[0], args[1])
		return nil
	}}
	transition := func(verb, action string) *cobra.Command {
		return &cobra.Command{Use: verb + " <name> <version>", Short: action, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			path := "/enterprise/v1/skills/" + args[0] + "/versions/" + args[1] + "/" + verb
			status, body, err := jsonRequest(cmd.Context(), o.Server, path, httpPost, bearer, map[string]any{})
			if err != nil {
				return err
			}
			if status < 200 || status >= 300 {
				return fmt.Errorf("skill %s failed (%d): %s", verb, status, redact(string(body)))
			}
			printHuman(o, "%s %s@%s", action, args[0], args[1])
			return nil
		}}
	}
	sync := &cobra.Command{
		Use:   "sync <name> <version>",
		Short: "Install/upgrade a released skill into the client root (verified + atomic)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			clientRoot, lockPath := resolve()
			keys, err := o.PackageKeys(cmd.Context(), o.Server)
			if err != nil {
				return err
			}
			dl := &gatewayDownloader{server: o.Server, bearer: bearer, workspace: o.Workspace}
			result, err := skillSync(cmd.Context(), dl, syncOptions(clientRoot, lockPath, client, keys), args[0], args[1], false)
			if err != nil {
				return err
			}
			printHuman(o, "skill %s@%s 已同步到 %s（lockfile %s）", result.Name, result.Version, clientRoot, lockPath)
			return nil
		},
	}
	remove := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a managed skill installation (policy removal)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			clientRoot, lockPath := resolve()
			result, err := skillSync(cmd.Context(), nil, syncOptions(clientRoot, lockPath, client, nil), args[0], "", true)
			if err != nil {
				return err
			}
			printHuman(o, "skill %s 已移除（仅托管文件）", result.Name)
			return nil
		},
	}
	testCmd := &cobra.Command{
		Use:   "test <dir>",
		Short: "Run the skill's declared test command (explicit invocation only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			declared, err := declaredTestCommand(args[0])
			if err != nil {
				return err
			}
			return runSkillTest(cmd.Context(), args[0], declared, o)
		},
	}
	search := &cobra.Command{
		Use:   "search [query]",
		Short: "List skills visible to the Workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			q := ""
			if len(args) == 1 {
				q = args[0]
			}
			rows, err := skillCatalog(cmd.Context(), o, bearer, q)
			if err != nil {
				return err
			}
			if o.JSON {
				printJSON(o, map[string]any{"skills": rows})
				return nil
			}
			for _, r := range rows {
				printHuman(o, "%s@%s\t%s", r["name"], r["version"], r["status"])
			}
			return nil
		},
	}
	info := &cobra.Command{
		Use:   "info <name>",
		Short: "Show registry versions and statuses of one skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			rows, err := skillCatalog(cmd.Context(), o, bearer, args[0])
			if err != nil {
				return err
			}
			var mine []map[string]any
			for _, r := range rows {
				if r["name"] == args[0] {
					mine = append(mine, r)
				}
			}
			if len(mine) == 0 {
				return fmt.Errorf("skill %q is not visible in this Workspace", args[0])
			}
			if o.JSON {
				printJSON(o, map[string]any{"skill": args[0], "versions": mine})
				return nil
			}
			for _, r := range mine {
				printHuman(o, "%s@%s\t%s\tdigest %s", r["name"], r["version"], r["status"], r["digest"])
			}
			return nil
		},
	}
	install := &cobra.Command{
		Use:   "install <name>[@version]",
		Short: "Install a released skill (latest released when no version given)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			name, version := splitNameVersion(args[0])
			clientRoot, lockPath := resolve()
			keys, err := o.PackageKeys(cmd.Context(), o.Server)
			if err != nil {
				return err
			}
			if version == "" {
				version, err = latestReleasedVersion(cmd.Context(), o, bearer, name)
				if err != nil {
					return err
				}
			}
			dl := &gatewayDownloader{server: o.Server, bearer: bearer, workspace: o.Workspace}
			result, err := skillSync(cmd.Context(), dl, syncOptions(clientRoot, lockPath, client, keys), name, version, false)
			if err != nil {
				return err
			}
			printHuman(o, "skill %s@%s 已安装到 %s（lockfile %s）", result.Name, result.Version, clientRoot, lockPath)
			return nil
		},
	}
	update := &cobra.Command{
		Use:   "update",
		Short: "Upgrade every lockfile-managed skill of this client to its latest released version",
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			clientRoot, lockPath := resolve()
			lock, err := skillsync.LoadLockfile(lockPath)
			if err != nil {
				return err
			}
			keys, err := o.PackageKeys(cmd.Context(), o.Server)
			if err != nil {
				return err
			}
			dl := &gatewayDownloader{server: o.Server, bearer: bearer, workspace: o.Workspace}
			changed := 0
			for _, e := range lock.Entries {
				if e.Client != client {
					continue
				}
				latest, ok, err := latestReleasedVersionOk(cmd.Context(), o, bearer, e.Name)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				if latest == e.Version {
					continue
				}
				if _, err := skillSync(cmd.Context(), dl, syncOptions(clientRoot, lockPath, client, keys), e.Name, latest, false); err != nil {
					return err
				}
				changed++
			}
			printHuman(o, "skill update 完成：%d 个已升级", changed)
			return nil
		},
	}
	c.AddCommand(init, validate, testCmd, pack, publish, transition("review", "已审核"), transition("release", "已发布"), transition("deprecate", "已弃用"), transition("revoke", "已吊销"), sync, install, update, remove, search, info)
	return c
}

// declaredTestCommand parses the developer checkout's skill.yaml and returns
// the declared test command. skill.yaml content is never executed at install
// or sync time — only this explicit command may run it.
func declaredTestCommand(dir string) (string, error) {
	v := viper.New()
	v.SetConfigFile(filepath.Join(dir, "skill.yaml"))
	if err := v.ReadInConfig(); err != nil {
		return "", fmt.Errorf("cannot read skill.yaml in %s: %w", dir, err)
	}
	declared := strings.TrimSpace(v.GetString("test"))
	if declared == "" {
		return "", fmt.Errorf("skill.yaml does not declare a test command")
	}
	return declared, nil
}

// runSkillTest executes the declared command with cwd pinned to the checkout.
func runSkillTest(ctx context.Context, dir, declared string, o *Options) error {
	var cmdExec *exec.Cmd
	if runtime.GOOS == "windows" {
		cmdExec = exec.CommandContext(ctx, "cmd", "/c", declared)
	} else {
		cmdExec = exec.CommandContext(ctx, "sh", "-c", declared)
	}
	cmdExec.Dir = dir
	cmdExec.Stdout = o.Stdout
	cmdExec.Stderr = o.Stderr
	if err := cmdExec.Run(); err != nil {
		return fmt.Errorf("skill test failed: %v", err)
	}
	printHuman(o, "skill test 通过")
	return nil
}

// skillCatalog fetches the workspace catalog rows (optionally filtered).
func skillCatalog(ctx context.Context, o *Options, bearer, q string) ([]map[string]any, error) {
	path := "/enterprise/v1/skills"
	if q != "" {
		path += "?q=" + url.QueryEscape(q)
	}
	req, err := httpNewRequestWithContext(ctx, httpGet, o.Server+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Agent-Workspace", o.Workspace)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("skill catalog failed (%d): %s", resp.StatusCode, redact(string(body)))
	}
	var doc struct {
		Skills []map[string]any `json:"skills"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("skill catalog is corrupt: %v", err)
	}
	return doc.Skills, nil
}

// latestReleasedVersion resolves the highest released catalog version.
func latestReleasedVersion(ctx context.Context, o *Options, bearer, name string) (string, error) {
	v, ok, err := latestReleasedVersionOk(ctx, o, bearer, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no released version of skill %q", name)
	}
	return v, nil
}

func latestReleasedVersionOk(ctx context.Context, o *Options, bearer, name string) (string, bool, error) {
	rows, err := skillCatalog(ctx, o, bearer, name)
	if err != nil {
		return "", false, err
	}
	best := ""
	found := false
	for _, r := range rows {
		if r["name"] != name || r["status"] != skillreg.StatusReleased {
			continue
		}
		version, _ := r["version"].(string)
		if !found || skillsync.CompareVersions(version, best) > 0 {
			best, found = version, true
		}
	}
	return best, found, nil
}

// splitNameVersion parses "name" or "name@version".
func splitNameVersion(arg string) (string, string) {
	if i := strings.LastIndex(arg, "@"); i > 0 {
		return arg[:i], arg[i+1:]
	}
	return arg, ""
}
