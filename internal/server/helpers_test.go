package server

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/CherryHQ/stella/internal/agent"
	"github.com/CherryHQ/stella/internal/agent/prompt"
	agentruntime "github.com/CherryHQ/stella/internal/agent/runtime"
	sessionaccess "github.com/CherryHQ/stella/internal/agent/session/access"
	"github.com/CherryHQ/stella/internal/asset"
	"github.com/CherryHQ/stella/internal/auth"
	"github.com/CherryHQ/stella/internal/auth/account"
	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/channel"
	"github.com/CherryHQ/stella/internal/connections"
	oauth "github.com/CherryHQ/stella/internal/connections/oauth"
	"github.com/CherryHQ/stella/internal/controlplane"
	agentaccess "github.com/CherryHQ/stella/internal/core/access"
	"github.com/CherryHQ/stella/internal/credential"
	appdb "github.com/CherryHQ/stella/internal/db"
	"github.com/CherryHQ/stella/internal/db/dbtest"
	"github.com/CherryHQ/stella/internal/inbox"
	"github.com/CherryHQ/stella/internal/library/recally"
	"github.com/CherryHQ/stella/internal/memory"
	"github.com/CherryHQ/stella/internal/memory/memorywrite"
	memprofile "github.com/CherryHQ/stella/internal/memory/profile"
	oauthserver "github.com/CherryHQ/stella/internal/oidc"
	"github.com/CherryHQ/stella/internal/platform/config"
	"github.com/CherryHQ/stella/internal/platform/home"
	"github.com/CherryHQ/stella/internal/plugin"
	"github.com/CherryHQ/stella/internal/plugin/host"
	sharepkg "github.com/CherryHQ/stella/internal/share"
	"github.com/CherryHQ/stella/internal/skill"
	"github.com/CherryHQ/stella/internal/skill/access"
	"github.com/CherryHQ/stella/pkg/db/sqlc"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	"github.com/CherryHQ/stella/pkg/providers"
	"github.com/CherryHQ/stella/plugins/email"
)

func testProviderRegistry(t *testing.T) *providers.Registry {
	t.Helper()
	build := func(providers.Config) (providers.ProviderAdapter, error) { return nil, nil }
	registry, err := providers.NewRegistry(
		providers.Definition{ID: "anthropic", Name: "Anthropic", DefaultURL: "https://api.anthropic.com", Build: build},
		providers.Definition{ID: "openai", Name: "OpenAI", DefaultURL: "https://api.openai.com/v1", Build: build},
		providers.Definition{ID: "openai-response", Name: "OpenAI Response", DefaultURL: "https://api.openai.com/v1", Build: build},
	)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// serverTestWorkspace maps each helper fixture's temporary legacy Home layout.
// It is explicit test composition, never a production fallback.
type serverTestWorkspace struct{ root string }

func (w serverTestWorkspace) WorkspaceView(_ context.Context, req home.WorkspaceRequest) (home.WorkspaceView, error) {
	principal := filepath.Join(w.root, "users", req.UserID)
	if req.GroupID != "" {
		principal = filepath.Join(w.root, "users", "group-"+req.GroupID)
	}
	data, agentRoot := filepath.Join(principal, "data"), filepath.Join(principal, "agents", req.AgentID)
	if err := os.MkdirAll(filepath.Join(data, "assets"), 0o755); err != nil {
		return home.WorkspaceView{}, err
	}
	if err := os.MkdirAll(agentRoot, 0o755); err != nil {
		return home.WorkspaceView{}, err
	}
	return home.WorkspaceView{PrincipalRoot: principal, DataRoot: data, AgentRoot: agentRoot}, nil
}

func (w serverTestWorkspace) OpenRoot(ctx context.Context, req home.WorkspaceRequest, scope home.RootScope, _ home.RootAccess) (home.RootOperations, error) {
	view, err := w.WorkspaceView(ctx, req)
	if err != nil {
		return nil, err
	}
	dir := view.AgentRoot
	if scope == home.RootPrincipalData {
		dir = view.DataRoot
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return serverRootOperations{Root: r}, nil
}

type serverRootOperations struct{ *os.Root }

func (r serverRootOperations) Close() error { return r.Root.Close() }
func (r serverRootOperations) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	return r.Root.Stat(name)
}

func (r serverRootOperations) List(_ context.Context, name string, o home.ListOptions) ([]fs.DirEntry, error) {
	d, err := r.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	f, err := d.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.ReadDir(o.Limit)
}

func (r serverRootOperations) Read(_ context.Context, name string, dst io.Writer, o home.ReadOptions) error {
	f, err := r.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(dst, io.LimitReader(f, o.MaxBytes))
	return err
}

func (r serverRootOperations) Write(_ context.Context, name string, src io.Reader, o home.WriteOptions) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	return r.WriteFile(name, data, o.Mode)
}

func (r serverRootOperations) Upload(ctx context.Context, name string, src io.Reader, o home.WriteOptions) error {
	return r.Write(ctx, name, src, o)
}

func (r serverRootOperations) Mkdir(_ context.Context, name string, mode fs.FileMode, o home.MkdirOptions) error {
	if o.Parents {
		return r.MkdirAll(name, mode)
	}
	return r.Root.Mkdir(name, mode)
}

func (r serverRootOperations) Remove(_ context.Context, name string, o home.RemoveOptions) error {
	if o.Recursive {
		return r.RemoveAll(name)
	}
	return r.Root.Remove(name)
}

func (r serverRootOperations) Rename(_ context.Context, old, new string, _ home.RenameOptions) error {
	return r.Root.Rename(old, new)
}

// testTransportOwnerDeletion preserves the HTTP fixture's historical database
// delete behavior. It is test-only: production uses Home OwnerDeletion.
type testTransportOwnerDeletion struct {
	agents interface {
		DeleteAgent(context.Context, string) error
	}
}

type testAgentIDOccupancy struct{}

func (testAgentIDOccupancy) AgentIDOccupied(context.Context, string) (bool, error) { return false, nil }

func (d testTransportOwnerDeletion) DeleteAgent(ctx context.Context, id, _ string) error {
	return d.agents.DeleteAgent(ctx, id)
}

// testServerDeps builds a full, valid Deps mirroring what the composition root
// assembles — the same shared instances, no shadow construction. Optional
// capabilities are left nil so their endpoints 503.
func testServerDeps(t *testing.T, store config.Store, as *appdb.AuthStore, mem memory.Provider, db *pgxpool.Pool, phost *host.Host) Deps {
	t.Helper()
	const baseURL = "http://localhost:25678"
	oidcStore := appdb.NewOIDCStore(db)
	authSvc := auth.NewAuthService(db, oidcStore, oidcStore, oidcStore)
	sessionMgr, err := auth.NewSessionManager(oidcStore, "test-vault-key")
	if err != nil {
		t.Fatalf("NewSessionManager: %v", err)
	}
	recallyStore := recally.NewStore(db)
	credLog := slog.With("component", "admin-test")
	credPATStore := credential.NewPostgresStore(db)
	oauthStore := oauthserver.NewPostgresStore(db)
	credFrontDoor := credential.NewService(credential.Config{PATs: credPATStore, OAuth: oauthStore, Users: credPATStore, Logger: credLog})
	oauthAuthServer := oauthserver.NewService(oauthserver.Config{Store: oauthStore, Issuer: credFrontDoor, Logger: credLog})
	changelogPageReader, ok := mem.(memory.ChangelogPageReader)
	if !ok {
		t.Fatal("test memory provider does not implement ChangelogPageReader")
	}
	assetHome := t.TempDir()
	assetStore, err := asset.NewStore(assetHome, nil, nil)
	if err != nil {
		t.Fatalf("asset.NewStore: %v", err)
	}
	poolMgr := agent.NewPoolManager(store, mem)
	credSvc := connections.NewService(nil, sqlc.New(db), oauth.NewFlowStore(), baseURL)
	agentAccess := agentaccess.NewService(store, as)
	homeManager, err := home.NewWorkspaceManager(db, t.TempDir())
	if err != nil {
		t.Fatalf("home.NewWorkspaceManager: %v", err)
	}
	t.Cleanup(func() { _ = homeManager.Close() })
	skillStore, err := skill.NewPOSIXStore(db, homeManager)
	if err != nil {
		t.Fatalf("skill.NewPOSIXStore: %v", err)
	}
	skillAccess := access.NewService(skillStore, agentAccess)
	projectStore := agent.NewProjectStore(db, agentAccess, agent.WithProjectHomeWorkspace(serverTestWorkspace{root: config.StellaHome()}))
	systemPromptBuilder, err := sessionaccess.NewSystemPromptBuilder(sessionaccess.SystemPromptDeps{
		Memory:    mem,
		Agents:    sessionaccess.ConfigAgentSystemPrompt(store),
		Projects:  projectStore.Resolve,
		Workspace: serverTestWorkspace{root: config.StellaHome()},
		PluginContextBuilder: func(context.Context, authz.Authority, string) (agentruntime.PluginContext, error) {
			return agentruntime.PluginContext{}, nil
		},
		PromptSectionsBuilder: func(context.Context, pkgplugins.SystemPromptContext, plugin.Snapshot) ([]pkgplugins.SystemPromptSection, error) {
			return nil, nil
		},
		Skills: func(ctx context.Context, build pkgplugins.SystemPromptContext, project *skill.ProjectSnapshot) (pkgplugins.SystemPromptSection, error) {
			return skill.BuildAuthorizedPromptSection(ctx, build, project, skillStore, skillAccess)
		},
	})
	if err != nil {
		t.Fatalf("sessionaccess.NewSystemPromptBuilder: %v", err)
	}
	sessionSvc, err := sessionaccess.NewService(mem, db, store, assetStore, agentAccess, sessionaccess.WithSystemPromptBuilder(systemPromptBuilder))
	if err != nil {
		t.Fatalf("sessionaccess.NewService: %v", err)
	}
	toolOverrides := agent.NewToolOverrideStore(db)
	agentSkillPolicy, _ := store.(AgentSkillPolicyStore)
	agentManagement := agentaccess.NewManagement(agentAccess, store, as, poolMgr, testUserDirectory{users: oidcStore}, agent.NewAgentActivityStore(db), nil, nil, slog.With("component", "agent-management-test"), agentaccess.WithOwnerDeletion(testTransportOwnerDeletion{agents: store}), agentaccess.WithAgentIDOccupancy(testAgentIDOccupancy{}))
	accountSvc := account.NewService(oidcStore, oidcStore, oidcStore, oidcStore, oidcStore, as, credFrontDoor, slog.With("component", "account-test"))
	memProfiles, _ := mem.(memory.ProfileStore)
	memChangelog, _ := mem.(memory.ChangelogReader)
	memoryManagement := memorywrite.NewManagementService(db, changelogPageReader)
	profileSvc := memprofile.NewService(db, memProfiles, memChangelog, memoryManagement, agentAccess, prompt.DefaultAgentSoul, slog.With("component", "profile-test"))
	return Deps{
		Pinger:               db,
		Group:                channel.NewGroupService(db, agentAccess, channel.NewRuntimeResolver(store), nil, nil),
		Account:              accountSvc,
		Profile:              profileSvc,
		ProjectStore:         projectStore,
		Inbox:                inbox.NewService(db),
		AgentAccess:          agentAccess,
		AgentManagement:      agentManagement,
		AgentSkillPolicy:     agentSkillPolicy,
		ToolOverrides:        toolOverrides,
		SessionAccess:        sessionSvc,
		SkillAccess:          skillAccess,
		Skills:               skillStore,
		LinkCodes:            auth.NewLinkCodeStore(),
		PoolManager:          poolMgr,
		PluginHost:           phost,
		WeixinRegistrar:      NewTestWeixinRegistrar(),
		BaseURL:              baseURL,
		Credentials:          credSvc,
		ControlPlane:         controlplane.NewService(store, phost, testProviderRegistry(t), poolMgr, credSvc, slog.With("component", "controlplane-test")),
		Email:                email.NewService(host.ResolveEmailUser, nil, sqlc.New(db)),
		EmailConfigValidator: email.ValidateConfigValue,
		Share:                sharepkg.NewService(sqlc.New(db), mem, recallyStore, assetHome, baseURL, sharepkg.WithHomeWorkspace(serverTestWorkspace{root: config.StellaHome()}), sharepkg.WithAgentAccess(agentAccess)),
		Recally:              recally.NewService(recallyStore, t.TempDir()),
		CredentialFrontDoor:  credFrontDoor,
		OAuthAuthServer:      oauthAuthServer,
		Assets:               assetStore,
		OIDC: OIDCDeps{
			AuthSvc:    authSvc,
			SessionMgr: sessionMgr,
		},
	}
}

// testUserDirectory adapts the OIDC user store to the Agent domain's
// UserDirectory port for tests, mirroring the composition-root adapter.
type testUserDirectory struct {
	users interface {
		GetUser(ctx context.Context, id string) (auth.User, error)
	}
}

func (d testUserDirectory) LookupUser(ctx context.Context, id string) (agentaccess.UserRef, error) {
	u, err := d.users.GetUser(ctx, id)
	if err != nil {
		return agentaccess.UserRef{}, err
	}
	return agentaccess.UserRef{ID: u.ID, Email: u.Email}, nil
}

func (d testUserDirectory) LookupUsers(ctx context.Context, ids []string) ([]agentaccess.UserRef, error) {
	out := make([]agentaccess.UserRef, 0, len(ids))
	for _, id := range ids {
		u, err := d.users.GetUser(ctx, id)
		if err != nil {
			continue
		}
		out = append(out, agentaccess.UserRef{ID: u.ID, Email: u.Email})
	}
	return out, nil
}

func TestResolvedToDBSkillPreservesExactRevisionIdentity(t *testing.T) {
	const digest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	resolved := &skill.ResolvedSkill{Skill: skill.Skill{
		ID: "skill-id", Scope: "user_agent", UserID: "user-id", AgentID: "agent-id",
		Name: "review-notes", ContentDigest: digest,
	}}
	got := resolvedToDBSkill(resolved)
	if got.ID != resolved.ID || got.Scope != resolved.Scope || got.UserID != resolved.UserID || got.AgentID != resolved.AgentID ||
		got.Name != resolved.Name || got.ContentDigest != digest {
		t.Fatalf("exact Home identity lost in conversion: %#v", got)
	}
}

func TestManagedSkillAgentFileLoadPreservesExactRevisionDigest(t *testing.T) {
	db := dbtest.New(t)
	manager, err := home.NewWorkspaceManager(db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	store, err := skill.NewPOSIXStore(db, manager)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.CreateManagedSkill(t.Context(), skill.Skill{
		ID: "server-exact-revision", Scope: "system", Name: "server-exact-revision",
	}, map[string]string{
		skill.MainFile: "# Server exact revision",
		"reference.md": "exact managed content",
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{skills: store}
	resolved := &skill.ResolvedSkill{Skill: snapshot.Skill}
	if resolved.ContentDigest != snapshot.Skill.ContentDigest {
		t.Fatalf("converted digest = %q, want %q", resolved.ContentDigest, snapshot.Skill.ContentDigest)
	}
	content, err := srv.loadSkillFile(t.Context(), resolved, "reference.md")
	if err != nil || content != "exact managed content" {
		t.Fatalf("managed file = %q, %v", content, err)
	}
}
