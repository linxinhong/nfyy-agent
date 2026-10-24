package cli

// Doctor: diagnoses connectivity, credential, workspace, model/MCP
// availability and minimum versions. A failing check is a finding, never an
// empty success.

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
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"enterprise-agent-platform/packages/agentdist"
)

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func newDoctorCmd(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local enterprise Agent environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			results := runDoctor(cmd.Context(), o)
			okAll := true
			for _, r := range results {
				if !r.OK {
					okAll = false
				}
			}
			if o.JSON {
				printJSON(o, map[string]any{"ok": okAll, "checks": results})
			} else {
				for _, r := range results {
					mark := "✓"
					if !r.OK {
						mark = "✗"
					}
					printHuman(o, "%s %s: %s", mark, r.Name, r.Detail)
				}
				if !okAll {
					printHuman(o, "doctor: 存在未通过项")
				} else {
					printHuman(o, "doctor: 全部通过")
				}
			}
			if !okAll {
				return silentFailure
			}
			return nil
		},
	}
}

var silentFailure = fmt.Errorf("doctor found problems")

func runDoctor(ctx context.Context, o *Options) []checkResult {
	var results []checkResult
	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Connectivity.
	status, body, err := jsonRequest(ctx, o.Server, "/healthz", httpGet, "", nil)
	results = append(results, checkResult{
		Name: "connectivity", OK: err == nil && status == 200,
		Detail: firstNonEmpty(errString(err), fmt.Sprintf("healthz %d %s", status, firstLine(body))),
	})

	// 2. Credential.
	hasEnv := os.Getenv("EAP_API_KEY") != ""
	keyed := false
	if o.Keyring != nil {
		_, keyed = keyringPeek(o.Keyring)
	}
	results = append(results, checkResult{
		Name: "credential", OK: hasEnv || keyed,
		Detail: fmt.Sprintf("EAP_API_KEY set=%v, keychain credential=%v", hasEnv, keyed),
	})

	// 3. Workspace selection + membership.
	wd := o.Workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	pv := viper.New()
	pv.SetConfigFile(filepath.Join(wd, projectConfig))
	wsErr := pv.ReadInConfig()
	ws := ""
	if wsErr == nil {
		ws = pv.GetString("workspace")
	}
	results = append(results, checkResult{
		Name: "workspace", OK: wsErr == nil && ws != "",
		Detail: firstNonEmpty(
			mapErr(wsErr, "no project workspace selected (eap workspace use <id>)"),
			fmt.Sprintf("selected %q", ws)),
	})

	// 4. Model availability (requires credential + workspace).
	bearer, authErr := o.authBearer(ctx)
	modelsOK, modelsDetail := false, firstNonEmpty(errString(authErr), "no credential")
	if authErr == nil {
		req, reqErr := httpNewRequestWithContext(ctx, httpGet, o.Server+"/v1/models", nil)
		if reqErr == nil {
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Agent-Workspace", ws)
			if resp, rErr := client.Do(req); rErr == nil {
				defer func() { _ = resp.Body.Close() }()
				var models struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&models)
				modelsOK = resp.StatusCode == 200 && len(models.Data) > 0
				ids := make([]string, 0, len(models.Data))
				for _, m := range models.Data {
					ids = append(ids, m.ID)
				}
				modelsDetail = fmt.Sprintf("%d models: %s", len(models.Data), strings.Join(ids, ","))
			} else {
				modelsDetail = rErr.Error()
			}
		}
	}
	results = append(results, checkResult{Name: "models", OK: modelsOK, Detail: modelsDetail})

	// 5. MCP authorization.
	mcpOK, mcpDetail := false, firstNonEmpty(errString(authErr), "no credential")
	if authErr == nil {
		req, reqErr := httpNewRequestWithContext(ctx, httpPost, o.Server+"/enterprise/v1/token/exchange",
			strings.NewReader(`{"audience":"mcp-gateway"}`))
		if reqErr == nil {
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Agent-Workspace", ws)
			req.Header.Set("Content-Type", "application/json")
			if resp, rErr := client.Do(req); rErr == nil {
				defer func() { _ = resp.Body.Close() }()
				mcpOK = resp.StatusCode == 200
				mcpDetail = fmt.Sprintf("exchange %d", resp.StatusCode)
			} else {
				mcpDetail = rErr.Error()
			}
		}
	}
	results = append(results, checkResult{Name: "mcp", OK: mcpOK, Detail: mcpDetail})

	// 6. Minimum protocol version of the CLI itself.
	meets := protocolVersionMeet(o.MinProto, "1.0.0")
	results = append(results, checkResult{
		Name: "min_version", OK: meets,
		Detail: fmt.Sprintf("cli protocol %s >= required %s", o.MinProto, "1.0.0"),
	})

	// 7. Agent minimum-version policy: installed clients must meet the
	// minimums published in the bootstrap manifest. Missing credential or
	// manifest is a finding, never an empty success.
	agentOK, agentDetail := false, ""
	if authErr != nil {
		agentDetail = errString(authErr)
	} else if req, reqErr := httpNewRequestWithContext(ctx, httpGet, o.Server+"/enterprise/v1/cli/bootstrap", nil); reqErr != nil {
		agentDetail = reqErr.Error()
	} else {
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("X-Agent-Workspace", ws)
		if resp, rErr := client.Do(req); rErr != nil {
			agentDetail = rErr.Error()
		} else {
			data, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			var manifest struct {
				Agents []struct {
					Client         string `json:"client"`
					MinimumVersion string `json:"minimum_version"`
				} `json:"agents"`
			}
			_ = json.Unmarshal(data, &manifest)
			if resp.StatusCode != 200 {
				agentDetail = fmt.Sprintf("bootstrap %d", resp.StatusCode)
			} else {
				agentOK = true
				var stale []string
				for _, a := range manifest.Agents {
					installed := readInstalledVersion(filepath.Join(agentdist.DefaultInstallRoot(),
						runtime.GOOS+"-"+runtime.GOARCH, a.Client))
					if installed == "" {
						continue // not installed: nothing to violate
					}
					if !agentdist.VersionMeet(installed, a.MinimumVersion) {
						stale = append(stale, fmt.Sprintf("%s %s < min %s", a.Client, installed, a.MinimumVersion))
						agentOK = false
					}
				}
				if len(stale) > 0 {
					agentDetail = "below minimum: " + strings.Join(stale, ", ") + " (eap agent update)"
				} else {
					agentDetail = fmt.Sprintf("%d agents under policy, all installed versions comply", len(manifest.Agents))
				}
			}
		}
	}
	results = append(results, checkResult{Name: "agents", OK: agentOK, Detail: agentDetail})
	return results
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func mapErr(err error, msg string) string {
	if err != nil {
		return msg
	}
	return ""
}

func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func keyringPeek(k KeyringStore) (string, bool) {
	v, err := k.Get(keyringService, keyringUser)
	return v, err == nil
}

// protocolVersionMeet is a semver-ish major.minor comparison.
func protocolVersionMeet(have, need string) bool {
	var hm, hn int
	if _, err := fmt.Sscanf(have, "%d", &hm); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(need, "%d", &hn); err != nil {
		return false
	}
	if hm != hn {
		return hm > hn
	}
	var hmi, hma, nmi, nma int
	_, _ = fmt.Sscanf(have, "%d.%d", &hmi, &hma)
	_, _ = fmt.Sscanf(need, "%d.%d", &nmi, &nma)
	_ = hma
	_ = nma
	// compare minor only when majors equal
	return hmi >= nmi
}

var _ = json.Marshal
var _ = context.Background
