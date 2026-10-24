package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Config precedence (ADR-0016): flags > environment (EAP_*) > project
// (.eap/workspace.yaml — never credentials) > user
// (~/.config/eap/config.yaml — never credentials).
const (
	envPrefix      = "EAP"
	projectConfig  = ".eap/workspace.yaml"
	userConfigDir  = ".config/eap"
	userConfigName = "config"
	keyringService = "eap-cli"
	keyringUser    = "refresh"
)

func newRootCmd(o *Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "eap",
		Short:         "Enterprise Agent Platform CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return resolveConfig(o, cmd)
		},
	}
	root.PersistentFlags().String("server", "http://127.0.0.1:18080", "gateway base URL")
	root.PersistentFlags().Bool("json", false, "machine-readable JSON output")
	_ = viper.BindPFlag("server", root.PersistentFlags().Lookup("server"))
	_ = viper.BindPFlag("json", root.PersistentFlags().Lookup("json"))

	root.AddCommand(
		newLoginCmd(o),
		newLogoutCmd(o),
		newWhoamiCmd(o),
		newAuthCmd(o),
		newWorkspaceCmd(o),
		newModelCmd(o),
		newMCPCmd(o),
		newSyncCmd(o),
		newSkillCmd(o),
		newAgentCmd(o),
		newConfigCmd(o),
		newDoctorCmd(o),
	)
	return root
}

// resolveConfig applies the precedence chain onto Options.
func resolveConfig(o *Options, cmd *cobra.Command) error {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	// environment layer
	_ = v.BindEnv("server")
	_ = v.BindEnv("api_key")

	// user config layer (~/.config/eap/config.yaml; never credentials)
	home, err := os.UserHomeDir()
	if err == nil {
		uv := viper.New()
		uv.SetConfigFile(filepath.Join(home, userConfigDir, userConfigName+".yaml"))
		if uv.ReadInConfig() == nil {
			for _, key := range []string{"server"} {
				if uv.IsSet(key) && !v.IsSet(key) {
					v.SetDefault(key, uv.Get(key))
				}
			}
		}
	}

	// project config layer (.eap/workspace.yaml upward search; never
	// credentials)
	wd := o.Workdir
	if wd == "" {
		wd, _ = os.Getwd()
	}
	pv := viper.New()
	pv.SetConfigFile(filepath.Join(wd, projectConfig))
	if pv.ReadInConfig() == nil {
		if pv.IsSet("server") && !v.IsSet("server") {
			v.SetDefault("server", pv.Get("server"))
		}
		if pv.IsSet("workspace") {
			v.SetDefault("workspace", pv.Get("workspace"))
		}
	}

	// flag layer wins over everything bound above.
	if f := cmd.Flags().Lookup("server"); f != nil && f.Changed {
		v.Set("server", f.Value.String())
	}
	if f := cmd.Flags().Lookup("json"); f != nil && f.Changed {
		v.Set("json", f.Value.String() == "true")
	} else if !v.IsSet("json") {
		v.SetDefault("json", false)
	}

	server := v.GetString("server")
	if server == "" {
		server = "http://127.0.0.1:18080"
	}
	o.Server = strings.TrimRight(server, "/")
	o.Workspace = v.GetString("workspace")
	o.JSON = v.GetBool("json")
	// Credentials: environment only (keychain is read on demand).
	if key := os.Getenv("EAP_API_KEY"); key != "" {
		o.APIKey = key
	}
	return nil
}

// authBearer returns the credential for API calls: explicit EAP_API_KEY wins,
// otherwise the keychain device refresh->access exchange is used by callers.
func (o *Options) authBearer(ctx context.Context) (string, error) {
	if o.APIKey != "" {
		return o.APIKey, nil
	}
	refresh, err := o.Keyring.Get(keyringService, keyringUser)
	if err != nil {
		return "", fmt.Errorf("%w: run 'eap login' or set EAP_API_KEY", ErrNotFound)
	}
	access, err := o.refreshAccess(ctx, o.Server, refresh)
	if err != nil {
		return "", err
	}
	return access, nil
}

func jsonRequest(ctx context.Context, server, path, method, bearer string, body any) (int, []byte, error) {
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := httpNewRequestWithContext(ctx, method, server+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("cannot reach the gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := new(strings.Builder)
	_, _ = copyIO(buf, resp.Body)
	return resp.StatusCode, []byte(buf.String()), nil
}

func newLoginCmd(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Device-flow login (stores the refresh credential in the OS Keychain)",
		RunE: func(cmd *cobra.Command, args []string) error {
			var start struct {
				DeviceCode      string `json:"device_code"`
				UserCode        string `json:"user_code"`
				VerificationURI string `json:"verification_uri"`
			}
			status, body, err := jsonRequest(cmd.Context(), o.Server, "/enterprise/v1/auth/device/code", httpPost, "", nil)
			if err != nil {
				return err
			}
			if status != 200 {
				return fmt.Errorf("device code request failed (%d): %s", status, redact(string(body)))
			}
			_ = json.Unmarshal(body, &start)
			printHuman(o, "在 Portal 打开 %s 并输入代码 %s 以批准登录", start.VerificationURI, start.UserCode)
			deadline := timeNow().Add(2 * timeMinute)
			for {
				if timeNow().After(deadline) {
					return errors.New("device login timed out")
				}
				sleepContext(cmd.Context(), 2*timeSecond)
				status, body, err = jsonRequest(cmd.Context(), o.Server, "/enterprise/v1/auth/device/token", httpPost, "", map[string]any{
					"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
					"device_code": start.DeviceCode,
				})
				if err != nil {
					return err
				}
				var tok struct {
					AccessToken  string `json:"access_token"`
					RefreshToken string `json:"refresh_token"`
					Error        string `json:"error"`
				}
				_ = json.Unmarshal(body, &tok)
				switch {
				case tok.RefreshToken != "":
					if o.Keyring == nil {
						return errors.New("OS keychain is not available in this environment")
					}
					if err := o.Keyring.Set(keyringService, keyringUser, tok.RefreshToken); err != nil {
						return fmt.Errorf("keychain unavailable: %v", err)
					}
					printHuman(o, "登录成功（刷新凭据已存入 OS Keychain）")
					return nil
				case tok.Error == "authorization_pending":
					continue
				case tok.Error == "slow_down":
					sleepContext(cmd.Context(), 2*timeSecond)
					continue
				default:
					return fmt.Errorf("device login failed: %s", tok.Error)
				}
			}
		},
	}
}

func newLogoutCmd(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the stored device credential and clear the keychain entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			refresh, err := o.Keyring.Get(keyringService, keyringUser)
			if err == nil {
				_, _, _ = jsonRequest(cmd.Context(), o.Server, "/enterprise/v1/auth/device/logout", httpPost, "",
					map[string]any{"refresh_token": refresh})
			}
			if err := o.Keyring.Delete(keyringService, keyringUser); err != nil {
				return err
			}
			printHuman(o, "已退出登录（凭据已吊销并清除）")
			return nil
		},
	}
}

func newWhoamiCmd(o *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated identity and workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, berr := o.authBearer(cmd.Context())
			if berr != nil {
				return berr
			}
			status, body, err := jsonRequest(cmd.Context(), o.Server, "/enterprise/v1/me", httpGet, bearer, nil)
			if err != nil {
				return err
			}
			if status != 200 {
				return fmt.Errorf("whoami failed (%d): %s", status, redact(string(body)))
			}
			var me struct {
				User struct {
					ID    string `json:"id"`
					Email string `json:"email"`
				} `json:"user"`
				Workspaces []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"workspaces"`
			}
			_ = json.Unmarshal(body, &me)
			if o.JSON {
				printJSON(o, me)
				return nil
			}
			printHuman(o, "%s (%s)", me.User.Email, me.User.ID)
			for _, ws := range me.Workspaces {
				printHuman(o, "  workspace: %s (%s)", ws.ID, ws.Name)
			}
			return nil
		},
	}
}

func newAuthCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "auth", Short: "Authentication status"}
	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show credential state (never the credential itself)",
		RunE: func(cmd *cobra.Command, args []string) error {
			envSet := os.Getenv("EAP_API_KEY") != ""
			keyed := false
			if o.Keyring != nil {
				if _, gerr := o.Keyring.Get(keyringService, keyringUser); gerr == nil {
					keyed = true
				}
			}
			out := map[string]any{
				"env_api_key": envSet,
				"keychain":    keyed,
			}
			if o.JSON {
				printJSON(o, out)
				return nil
			}
			printHuman(o, "EAP_API_KEY: %v / keychain 凭据: %v", envSet, keyed)
			return nil
		},
	})
	return c
}

func newWorkspaceCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "workspace", Short: "Workspace selection and status"}
	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List your workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, berr := o.authBearer(cmd.Context())
			if berr != nil {
				return berr
			}
			status, body, err := jsonRequest(cmd.Context(), o.Server, "/enterprise/v1/me", httpGet, bearer, nil)
			if err != nil {
				return err
			}
			if status != 200 {
				return fmt.Errorf("workspace list failed (%d): %s", status, redact(string(body)))
			}
			var me struct {
				Workspaces []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"workspaces"`
			}
			_ = json.Unmarshal(body, &me)
			if o.JSON {
				printJSON(o, me)
				return nil
			}
			for _, ws := range me.Workspaces {
				printHuman(o, "%s  %s", ws.ID, ws.Name)
			}
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "use <workspace>",
		Short: "Select the workspace for this project (writes .eap/workspace.yaml, never credentials)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wd := o.Workdir
			if wd == "" {
				wd, _ = os.Getwd()
			}
			dir := filepath.Join(wd, ".eap")
			if err := osMkdirAll(dir, 0o755); err != nil {
				return err
			}
			content := fmt.Sprintf("# eap project config (no credentials allowed here)\nworkspace: %s\n", args[0])
			if err := osWriteFile(filepath.Join(dir, "workspace.yaml"), []byte(content), 0o644); err != nil {
				return err
			}
			printHuman(o, "workspace 已选择: %s（配置写入 %s）", args[0], projectConfig)
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the current workspace selection",
		RunE: func(cmd *cobra.Command, args []string) error {
			wd := o.Workdir
			if wd == "" {
				wd, _ = os.Getwd()
			}
			pv := viper.New()
			pv.SetConfigFile(filepath.Join(wd, projectConfig))
			if err := pv.ReadInConfig(); err != nil {
				return &usageError{msg: "no workspace selected (run 'eap workspace use <id>')"}
			}
			if o.JSON {
				printJSON(o, map[string]any{"workspace": pv.Get("workspace")})
				return nil
			}
			printHuman(o, "workspace: %v", pv.Get("workspace"))
			return nil
		},
	})
	return c
}

func newModelCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "model", Short: "Logical model discovery"}
	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List models available to you",
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			req, rerr := httpNewRequestWithContext(cmd.Context(), httpGet, o.Server+"/v1/models", nil)
			if rerr != nil {
				return rerr
			}
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Agent-Workspace", o.Workspace)
			resp, err := httpClient().Do(req)
			if err != nil {
				return fmt.Errorf("cannot reach the gateway: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			buf := new(strings.Builder)
			_, _ = copyIO(buf, resp.Body)
			if resp.StatusCode != 200 {
				return fmt.Errorf("model list failed (%d): %s", resp.StatusCode, redact(buf.String()))
			}
			var models struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			_ = json.Unmarshal([]byte(buf.String()), &models)
			if o.JSON {
				printJSON(o, models)
				return nil
			}
			for _, m := range models.Data {
				printHuman(o, "%s", m.ID)
			}
			return nil
		},
	})
	return c
}

func newMCPCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "mcp", Short: "MCP discovery and status"}
	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List MCP servers from the bootstrap manifest",
		RunE: func(cmd *cobra.Command, args []string) error {
			bearer, err := o.authBearer(cmd.Context())
			if err != nil {
				return err
			}
			req, rerr := httpNewRequestWithContext(cmd.Context(), httpPost, o.Server+"/enterprise/v1/token/exchange", strings.NewReader(`{"audience":"mcp-gateway"}`))
			if rerr != nil {
				return rerr
			}
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("X-Agent-Workspace", o.Workspace)
			req.Header.Set("Content-Type", "application/json")
			resp, err := httpClient().Do(req)
			if err != nil {
				return fmt.Errorf("cannot reach the gateway: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			buf := new(strings.Builder)
			_, _ = copyIO(buf, resp.Body)
			if resp.StatusCode != 200 {
				return fmt.Errorf("mcp discovery failed (%d): %s", resp.StatusCode, redact(buf.String()))
			}
			out := map[string]any{"audience": "mcp-gateway", "status": "authorized"}
			if o.JSON {
				printJSON(o, out)
				return nil
			}
			printHuman(o, "mcp-gateway: 已授权")
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show MCP authorization status",
		RunE: func(cmd *cobra.Command, args []string) error {
			printHuman(o, "使用 'eap mcp list' 检查 MCP 授权")
			return nil
		},
	})
	return c
}

func newConfigCmd(o *Options) *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Show resolved configuration (redacted)"}
	c.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show resolved configuration (credentials redacted)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := map[string]any{
				"server":      o.Server,
				"json":        o.JSON,
				"api_key_set": os.Getenv("EAP_API_KEY") != "",
			}
			if o.JSON {
				printJSON(o, out)
				return nil
			}
			printHuman(o, "server: %s", o.Server)
			printHuman(o, "json: %v", o.JSON)
			printHuman(o, "api_key: [set=%v]", os.Getenv("EAP_API_KEY") != "")
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "path",
		Short: "Print config file locations",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			if o.JSON {
				printJSON(o, map[string]any{
					"user":    filepath.Join(home, userConfigDir, userConfigName+".yaml"),
					"project": projectConfig,
				})
				return nil
			}
			printHuman(o, "user config:    %s", filepath.Join(home, userConfigDir, userConfigName+".yaml"))
			printHuman(o, "project config: %s", projectConfig)
			return nil
		},
	})
	return c
}
