package sandbox

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/plugin/manifest"
	"github.com/CherryHQ/stella/internal/vault"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
)

func TestRunnerFilesystemPolicyMountsCoreAndSelectedMiseContext(t *testing.T) {
	stellaHome := t.TempDir()
	publicRoot := filepath.Join(stellaHome, ".mise-tools", "public", "selected")
	for _, dir := range []string{publicRoot, filepath.Join(stellaHome, ".mise-tools", "installs"), filepath.Join(stellaHome, ".mise-tools", "contexts", "other")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	corePlan := fixtureCoreRuntimePlan(t, stellaHome)
	plan := &manifest.BinaryInstallPlan{
		Identity:     "selected",
		PublicDir:    publicRoot,
		PublicBinDir: publicRoot,
	}
	policy, sources := runnerFilesystemPolicy(Paths{StellaHome: stellaHome, WorkspaceRoot: t.TempDir()}, Config{
		CoreRuntimePlan:   corePlan,
		ContextBinaryPlan: plan,
	})
	coreMount := pkgsandbox.MountStellaHome + "/bin"
	if got := sources[coreMount]; got != corePlan.PublicDir {
		t.Fatalf("core mount source = %q, want %q", got, corePlan.PublicDir)
	}
	optionalMount := pkgsandbox.MountStellaHome + "/.mise-tools/public/selected"
	if got := sources[optionalMount]; got != publicRoot {
		t.Fatalf("optional mount source = %q, want %q", got, publicRoot)
	}
	for _, mount := range policy.Mounts {
		if mount.SandboxPath == pkgsandbox.MountStellaHome+"/.mise-tools" || mount.SandboxPath == pkgsandbox.MountStellaHome+"/.mise-tools/contexts" || mount.SandboxPath == pkgsandbox.MountStellaHome+"/.mise-tools/contexts/other" || mount.SandboxPath == pkgsandbox.MountStellaHome+"/.mise-tools/installs" {
			t.Fatalf("policy exposed broad or foreign mise mount: %#v", policy.Mounts)
		}
	}
	if sources[coreMount] == sources[optionalMount] {
		t.Fatal("core and optional selection mounts must remain independent")
	}
}

type staticVaultEnv struct {
	env map[string]string
}

func (v staticVaultEnv) LoadEnvForAgent(context.Context, string, string) (map[string]string, error) {
	out := make(map[string]string, len(v.env))
	maps.Copy(out, v.env)
	return out, nil
}

func (v staticVaultEnv) ListAmbientSecretMetas(context.Context, string, string) ([]vault.AmbientSecretMeta, error) {
	return nil, nil
}

func requireSessionSecretValues(t *testing.T, values []string, present []string, absent []string) {
	t.Helper()
	got := make(map[string]struct{}, len(values))
	for _, value := range values {
		got[value] = struct{}{}
	}
	for _, value := range present {
		if _, ok := got[value]; !ok {
			t.Fatalf("session secret values = %#v, missing %q", values, value)
		}
	}
	for _, value := range absent {
		if _, ok := got[value]; ok {
			t.Fatalf("session secret values = %#v, unexpectedly contains %q", values, value)
		}
	}
	if len(values) != len(present) {
		t.Fatalf("session secret values = %#v, want exactly %#v", values, present)
	}
}

func TestRemapStellaHomePolicyPathUsesPOSIXContainerPaths(t *testing.T) {
	for _, tt := range []struct {
		name       string
		hostPath   string
		stellaHome string
		want       string
	}{
		{
			name:       "POSIX host root",
			hostPath:   "/srv/stella/users/u1/.mise-tools",
			stellaHome: "/srv/stella",
			want:       "/opt/stella/users/u1/.mise-tools",
		},
		{
			name:       "Windows host root",
			hostPath:   `C:\stella\users\u1\.mise-tools`,
			stellaHome: `C:\stella`,
			want:       "/opt/stella/users/u1/.mise-tools",
		},
		{
			name:       "outside stella home remains host path",
			hostPath:   `C:\outside\tools`,
			stellaHome: `C:\stella`,
			want:       `C:\outside\tools`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := remapStellaHomePolicyPath(tt.hostPath, tt.stellaHome); got != tt.want {
				t.Errorf("remapStellaHomePolicyPath(%q, %q) = %q, want %q", tt.hostPath, tt.stellaHome, got, tt.want)
			}
		})
	}
}

func TestBuildSandboxEnvDropsVaultStellaToken(t *testing.T) {
	env, err := buildSandboxEnv(context.Background(), Config{
		UserID:         "user-1",
		AgentID:        "agent-1",
		SessionID:      "session-1",
		VaultEnvLoader: staticVaultEnv{env: map[string]string{"STELLA_TOKEN": "stella_legacy", "OTHER": "ok"}},
	}, Paths{})
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	if _, ok := env["STELLA_TOKEN"]; ok {
		t.Fatal("legacy vault token must not be injected")
	}
	if got := env["OTHER"]; got != "ok" {
		t.Fatalf("OTHER = %q, want vault value", got)
	}
}

func TestBuildSandboxEnvLayersMiseSystemGlobalAndWorkspace(t *testing.T) {
	stellaHome := "/stella"
	userDataDir := "/stella/users/user-1/data"
	workspace := "/stella/users/user-1/agents/agent-1/projects/project-1"
	env, err := buildSandboxEnv(context.Background(), Config{
		UserID:         "user-1",
		VaultEnvLoader: staticVaultEnv{env: map[string]string{pkgsandbox.EnvRunnerPath: "/untrusted/bin"}},
	}, Paths{
		StellaHome:    stellaHome,
		WorkspaceRoot: workspace,
		UserDataDir:   userDataDir,
	})
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	if _, ok := env["MISE_SYSTEM_CONFIG_FILE"]; ok {
		t.Fatalf("MISE_SYSTEM_CONFIG_FILE must stay unset until a snapshot selection is overlaid, got %q", env["MISE_SYSTEM_CONFIG_FILE"])
	}
	if got, want := env["BASH_ENV"], filepath.Join(stellaHome, "bin", ".stella-shell-env"); got != want {
		t.Fatalf("BASH_ENV = %q, want %q", got, want)
	}
	if got := env[pkgsandbox.EnvRunnerPath]; got != "" {
		t.Fatalf("%s = %q, want empty runner-owned baseline", pkgsandbox.EnvRunnerPath, got)
	}
	wantConfigDir := filepath.Join(userDataDir, ".config", "mise")
	if got := env["MISE_CONFIG_DIR"]; got != wantConfigDir {
		t.Fatalf("MISE_CONFIG_DIR = %q, want %q", got, wantConfigDir)
	}
	if got, want := env["MISE_GLOBAL_CONFIG_FILE"], filepath.Join(wantConfigDir, "config.toml"); got != want {
		t.Fatalf("MISE_GLOBAL_CONFIG_FILE = %q, want %q", got, want)
	}
}

func TestBuildSandboxEnvDoesNotInjectLarkCLIStateDirs(t *testing.T) {
	env, err := buildSandboxEnv(context.Background(), Config{UserID: "user-1"}, Paths{
		WorkspaceRoot: "/stella/users/user-1/agents/agent-1",
		UserDataDir:   "/stella/users/user-1/data",
	})
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	for _, key := range []string{"LARKSUITE_CLI_CONFIG_DIR", "LARKSUITE_CLI_DATA_DIR"} {
		if value, ok := env[key]; ok {
			t.Fatalf("retired lark-cli state override %s=%q must not be injected", key, value)
		}
	}
}

func TestBuildSandboxEnvDoesNotRenderFilesystemRootsForGroups(t *testing.T) {
	env, err := buildSandboxEnv(context.Background(), Config{GroupID: "group-1"}, Paths{
		WorkspaceRoot: "/stella/users/group-group-1/agents/agent-1",
		UserDataDir:   "/stella/users/group-group-1/data",
	})
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	// Filesystem roots are rendered only by the selected backend after it knows
	// its actual mounts; the runner baseline must not guess the group view.
	for _, key := range []string{"HOME", "STELLA_USER_DIR", "STELLA_ASSETS_DIR", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		if got, ok := env[key]; ok {
			t.Errorf("group runner env must not set %s=%q", key, got)
		}
	}
}

func TestBuildSandboxEnvRecordsOnlyInjectedVaultSecretValues(t *testing.T) {
	secretValues := NewSessionSecretValues()
	env, err := buildSandboxEnv(context.Background(), Config{
		UserID:              "user-1",
		AgentID:             "agent-1",
		SessionID:           "session-1",
		VaultEnvLoader:      staticVaultEnv{env: map[string]string{"MY_SECRET": "vault-secret", "STELLA_HOME": "vault-home", "STELLA_TOKEN": "stella_legacy"}},
		SessionSecretValues: secretValues,
	}, Paths{StellaHome: "/runtime/stella"})
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	if got := env["MY_SECRET"]; got != "vault-secret" {
		t.Fatalf("MY_SECRET = %q, want vault-secret", got)
	}
	values := secretValues.Values()
	requireSessionSecretValues(t, values,
		[]string{"vault-secret"},
		[]string{"vault-home", "stella_legacy", "/runtime/stella"},
	)
}

func TestBuildSandboxEnvDeletesVaultTokenWhenScopedUnavailable(t *testing.T) {
	env, err := buildSandboxEnv(context.Background(), Config{
		UserID:         "user-1",
		AgentID:        "agent-1",
		VaultEnvLoader: staticVaultEnv{env: map[string]string{"STELLA_TOKEN": "stella_legacy"}},
	}, Paths{})
	if err != nil {
		t.Fatalf("buildSandboxEnv: %v", err)
	}
	if _, ok := env["STELLA_TOKEN"]; ok {
		t.Fatal("legacy vault token must not be injected")
	}
}

func TestBuildSandboxEnvVaultSecretOverridesOAuthSessionEnv(t *testing.T) {
	for _, tt := range []struct {
		name          string
		vaultSecret   bool
		wantToken     string
		wantOAuthBind bool
		wantRedacted  []string
		absentSecrets []string
	}{
		{
			name:          "vault secret wins",
			vaultSecret:   true,
			wantToken:     "vault_pat",
			wantRedacted:  []string{"vault_pat"},
			absentSecrets: []string{"oauth_access_token"},
		},
		{
			name:          "oauth injects without vault collision",
			wantToken:     "oauth_access_token",
			wantOAuthBind: true,
			wantRedacted:  []string{"oauth_access_token"},
			absentSecrets: []string{"vault_pat"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			userID := "user-1"
			store := newStubOAuthVaultStore()
			registry := oauth.NewProviderRegistry()
			registry.Register(oauth.ProviderConfig{ID: "github", VaultKey: oauth.VaultKeyGitHub})
			tm := oauth.NewTokenManager(store)
			tm.SetRegistry(registry)
			if err := oauth.SaveOAuthBundle(ctx, store, userID, oauth.VaultKeyGitHub, oauth.OAuthBundle{
				Version:         1,
				AccessToken:     "oauth_access_token",
				AccessExpiresAt: time.Now().Add(time.Hour),
			}); err != nil {
				t.Fatalf("SaveOAuthBundle: %v", err)
			}
			if tt.vaultSecret {
				if err := store.Set(ctx, userID, "GH_TOKEN", "vault_pat"); err != nil {
					t.Fatalf("Set GH_TOKEN: %v", err)
				}
			}
			secretValues := NewSessionSecretValues()
			oauthBindings := NewOAuthEnvBindings()
			env, err := buildSandboxEnv(ctx, Config{
				UserID:              userID,
				AgentID:             "agent-1",
				VaultEnvLoader:      store,
				SessionSecretValues: secretValues,
				TokenManager:        tm,
				OAuthEnvBindings:    oauthBindings,
				SessionEnvSpecs: []pkgplugins.SessionEnvSpec{
					{EnvVar: "GH_TOKEN", Source: pkgplugins.SessionEnvSource("oauth.access_token"), OAuthProviderID: "github"},
				},
			}, Paths{})
			if err != nil {
				t.Fatalf("buildSandboxEnv: %v", err)
			}
			if got := env["GH_TOKEN"]; got != tt.wantToken {
				t.Fatalf("GH_TOKEN = %q, want %q", got, tt.wantToken)
			}
			if _, ok := env[oauth.VaultKeyGitHub]; ok {
				t.Fatalf("%s must not appear in sandbox env", oauth.VaultKeyGitHub)
			}
			if got := oauthBindings.Has("GH_TOKEN"); got != tt.wantOAuthBind {
				t.Fatalf("OAuth binding recorded = %v, want %v", got, tt.wantOAuthBind)
			}
			requireSessionSecretValues(t, secretValues.Values(), tt.wantRedacted, tt.absentSecrets)
		})
	}
}

func TestOAuthBundleFieldBrandIsInjectableButNotSecret(t *testing.T) {
	bundle := &oauth.OAuthBundle{Brand: "feishu"}
	if got, ok := oauthBundleField(bundle, "brand"); !ok || got != "feishu" {
		t.Fatalf("oauthBundleField(brand) = (%q, %v), want (feishu, true)", got, ok)
	}
	if oauthSessionEnvFieldSecret("brand") {
		t.Fatal("brand must not be treated as secret material")
	}
}
