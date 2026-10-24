package cli

// `eap agent` (TASK-019, ADR-0018): list, install and update authorized
// Agent client releases through same-domain short-lived download tickets.
// Bytes are activated only after digest and EAP package signature
// verification; the local installation root is owned by the adapter, never
// by server metadata.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"enterprise-agent-platform/packages/agentdist"
)

// gatewayAgentCatalog adapts the public /enterprise/v1/agents surface to the
// agentdist engine. TicketRedeem is server-internal; the CLI redeems via the
// download endpoint in its DownloadFunc.
type gatewayAgentCatalog struct {
	o *Options
}

func (g *gatewayAgentCatalog) authed(ctx context.Context, method, path string, body io.Reader) ([]byte, int, error) {
	bearer, err := g.o.authBearer(ctx)
	if err != nil {
		return nil, 0, err
	}
	req, err := httpNewRequestWithContext(ctx, method, g.o.Server+path, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Agent-Workspace", g.o.Workspace)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot reach the gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return data, resp.StatusCode, nil
}

// List implements agentdist.Catalog (platform declared by this client).
func (g *gatewayAgentCatalog) List(ctx context.Context, _, client, goos, goarch string) ([]agentdist.Release, error) {
	data, status, err := g.authed(ctx, httpGet,
		fmt.Sprintf("/enterprise/v1/agents/%s?os=%s&arch=%s", client, goos, goarch), nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("agent list failed (%d): %s", status, redact(string(data)))
	}
	var doc struct {
		Releases []agentdist.Release `json:"releases"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("agent list is corrupt: %v", err)
	}
	return doc.Releases, nil
}

// TicketIssue implements agentdist.Catalog via POST .../ticket.
func (g *gatewayAgentCatalog) TicketIssue(ctx context.Context, _, client, version, goos, goarch, _ string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"version": version, "os": goos, "arch": goarch})
	data, status, err := g.authed(ctx, httpPost,
		fmt.Sprintf("/enterprise/v1/agents/%s/ticket", client), strings.NewReader(string(payload)))
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("ticket issuance failed (%d): %s", status, redact(string(data)))
	}
	var doc struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Ticket == "" {
		return "", fmt.Errorf("ticket response is corrupt")
	}
	return doc.Ticket, nil
}

// TicketRedeem is not used client-side; downloads redeem the ticket through
// the download endpoint and carry digest/signature headers.
func (g *gatewayAgentCatalog) TicketRedeem(context.Context, string) ([]byte, string, []byte, error) {
	return nil, "", nil, fmt.Errorf("ticket redemption is a gateway-internal path")
}

func newAgentCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "agent", Short: "List, install and update authorized Agent clients"}
	var root string
	c.PersistentFlags().StringVar(&root, "root", "", "install root (default ~/.eap/agents, owned by the local adapter)")
	resolveRoot := func() string {
		if root != "" {
			return root
		}
		return agentdist.DefaultInstallRoot()
	}
	install := func(cmd *cobra.Command, client, version string) error {
		ctx := cmd.Context()
		catalog := &gatewayAgentCatalog{o: o}
		goos, goarch := runtime.GOOS, runtime.GOARCH
		if version == "" {
			rels, err := catalog.List(ctx, o.Workspace, client, goos, goarch)
			if err != nil {
				return err
			}
			if len(rels) == 0 {
				return fmt.Errorf("no released %s version for %s/%s in this Workspace", client, goos, goarch)
			}
			version = rels[0].Version // listed ORDER BY version DESC
		}
		keys, err := o.PackageKeys(ctx, o.Server)
		if err != nil {
			return err
		}
		res, err := agentdist.Install(ctx, catalog, agentdist.InstallOptions{
			Client: client, Version: version, InstallRoot: resolveRoot(),
			Keys: keys, GOOS: goos, GOARCH: goarch,
		}, func(ictx context.Context, ticket string) ([]byte, string, []byte, error) {
			bearer, err := o.authBearer(ictx)
			if err != nil {
				return nil, "", nil, err
			}
			payload, _ := json.Marshal(map[string]string{"ticket": ticket})
			req, err := httpNewRequestWithContext(ictx, httpPost,
				o.Server+fmt.Sprintf("/enterprise/v1/agents/%s/download", client), strings.NewReader(string(payload)))
			if err != nil {
				return nil, "", nil, err
			}
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Agent-Workspace", o.Workspace)
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient().Do(req)
			if err != nil {
				return nil, "", nil, fmt.Errorf("cannot reach the gateway: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, "", nil, err
			}
			if resp.StatusCode != http.StatusOK {
				return nil, "", nil, fmt.Errorf("agent download failed (%d): %s", resp.StatusCode, redact(string(data)))
			}
			digest := strings.TrimPrefix(resp.Header.Get("X-Agent-Digest"), "sha256-")
			sig := resp.Header.Get("X-Agent-Signature")
			if digest == "" || sig == "" {
				return nil, "", nil, fmt.Errorf("agent download is missing digest/signature headers")
			}
			return data, digest, []byte(sig), nil
		})
		if err != nil {
			return err
		}
		if err := recordInstalledVersion(res.Binary, version); err != nil {
			return err
		}
		printHuman(o, "agent %s@%s 已激活：%s", res.Client, res.Version, res.Binary)
		return nil
	}
	list := &cobra.Command{
		Use:   "list <client>",
		Short: "List released versions for this platform and Workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rels, err := (&gatewayAgentCatalog{o: o}).List(cmd.Context(), o.Workspace, args[0], runtime.GOOS, runtime.GOARCH)
			if err != nil {
				return err
			}
			if o.JSON {
				printJSON(o, map[string]any{"releases": rels})
				return nil
			}
			for _, r := range rels {
				printHuman(o, "%s@%s\tmin %s\t%s", r.Client, r.Version, r.MinimumVersion, r.Notes)
			}
			return nil
		},
	}
	installCmd := &cobra.Command{
		Use:   "install <client> [version]",
		Short: "Install a verified release (latest released when no version given)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			version := ""
			if len(args) == 2 {
				version = args[1]
			}
			return install(cmd, args[0], version)
		},
	}
	updateCmd := &cobra.Command{
		Use:   "update <client>",
		Short: "Update an installed client to the latest released version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := args[0]
			have := readInstalledVersion(filepath.Join(resolveRoot(), runtime.GOOS+"-"+runtime.GOARCH, client))
			rels, err := (&gatewayAgentCatalog{o: o}).List(cmd.Context(), o.Workspace, client, runtime.GOOS, runtime.GOARCH)
			if err != nil {
				return err
			}
			if len(rels) == 0 {
				return fmt.Errorf("no released %s version for %s/%s in this Workspace", client, runtime.GOOS, runtime.GOARCH)
			}
			latest := rels[0]
			if have != "" && have == latest.Version {
				printHuman(o, "agent %s@%s 已是最新", client, have)
				return nil
			}
			if have != "" && !agentdist.VersionMeet(have, latest.MinimumVersion) {
				printHuman(o, "agent %s@%s 低于最低版本 %s，必须升级", client, have, latest.MinimumVersion)
			}
			return install(cmd, client, latest.Version)
		},
	}
	c.AddCommand(list, installCmd, updateCmd)
	return c
}

// installedVersionPath/record/read keep the local installed-version state
// next to the binary (adapter-owned, no server metadata).
func installedVersionPath(binary string) string {
	return filepath.Join(filepath.Dir(binary), "VERSION")
}

func recordInstalledVersion(binary, version string) error {
	if binary == "" {
		return nil
	}
	return os.WriteFile(installedVersionPath(binary), []byte(version), 0o644)
}

func readInstalledVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "VERSION"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
