package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/CherryHQ/stella/internal/authz"
	pkgplugins "github.com/CherryHQ/stella/pkg/plugins"
	pkgsandbox "github.com/CherryHQ/stella/pkg/sandbox"
	"github.com/CherryHQ/stella/pkg/tools"
)

const runtimeUsageTouchTimeout = 500 * time.Millisecond

type Tool struct {
	svc             *Service
	runtime         RuntimeReader
	session         sandboxSession
	projectSnapshot *ProjectSnapshot
	projectionMu    sync.Mutex
	// Plugin visibility is captured at runner construction so tool search and
	// prompt search instructions use the same visible system-skill set.
	registeredPluginIDs []string
	enabledPluginIDs    []string
	disabledSkillRefs   []string
	// readAuthz enforces Skill read authorization on every managed Skill before
	// Home content is opened. It is mandatory at construction.
	readAuthz SkillReadAuthorizer
}

func (t *Tool) WithProjectSnapshot(snapshot *ProjectSnapshot) *Tool {
	t.projectSnapshot = snapshot
	return t
}

// errSkillNotFound is the opaque result of a denied or missing DB skill read; the
// tool never distinguishes "forbidden" from "missing" to the model.
var errSkillNotFound = errors.New("skill not found")

type sandboxSession interface {
	Files() pkgsandbox.FileAccess
}

// NewTool constructs the read-only runtime Skill tool. Exact Home authority,
// the active Session filesystem capability, and Skill authorization are one
// mandatory contract; construction never creates a reduced or fallback tool.
func NewTool(runtime RuntimeReader, session sandboxSession, authorizer SkillReadAuthorizer) (*Tool, error) {
	if runtime == nil || session == nil || authorizer == nil {
		return nil, errors.New("skills tool requires runtime, Session, and read authorizer")
	}
	return &Tool{svc: NewService(), runtime: runtime, session: session, readAuthz: authorizer}, nil
}

// WithPluginVisibility limits plugin-owned system skills to enabled plugins.
func (t *Tool) WithPluginVisibility(registered, enabled []string) *Tool {
	t.registeredPluginIDs = append([]string(nil), registered...)
	t.enabledPluginIDs = append([]string(nil), enabled...)
	return t
}

// WithAgentSkillPolicy captures the runner's immutable policy snapshot. The
// next runner observes a committed mutation after local invalidation.
func (t *Tool) WithAgentSkillPolicy(disabled []string) *Tool {
	t.disabledSkillRefs = append([]string(nil), disabled...)
	return t
}

// authorizeReadable filters merged skills through the read PEP: filesystem
// project/built-in skills pass unchanged; each DB row is authorized under one
// evaluation.
func (t *Tool) authorizeReadable(ctx context.Context, merged []ResolvedSkill) ([]ResolvedSkill, error) {
	anyDB := slices.ContainsFunc(merged, isDBSkill)
	if !anyDB {
		return merged, nil
	}
	out := make([]ResolvedSkill, 0, len(merged))
	var dec SkillReadDecision
	d, err := t.readAuthz.BeginRead(ctx)
	switch {
	case err == nil:
		dec = d
	case errors.Is(err, authz.ErrUnauthenticated):
		// No authorizable identity (e.g. a group turn without a group id): drop
		// DB rows (fail hidden) rather than failing the whole read; immutable
		// project/built-in skills below still pass.
		dec = nil
	default:
		return nil, err
	}
	for _, rs := range merged {
		if !isDBSkill(rs) {
			out = append(out, rs)
			continue
		}
		if dec == nil {
			continue // fail closed: no decider, drop the DB row
		}
		allowed, err := dec.AllowRead(ctx, rs.ID, rs.Scope, rs.UserID, rs.AgentID)
		if err != nil {
			return nil, err
		}
		if allowed {
			out = append(out, rs)
		}
	}
	return out, nil
}

// authorizeLoadable authorizes a single resolved skill for load. A denied or
// unauthorized DB row is reported not-found; filesystem skills pass.
func (t *Tool) authorizeLoadable(ctx context.Context, rs *ResolvedSkill) error {
	if rs == nil || !isDBSkill(*rs) {
		return nil
	}
	dec, err := t.readAuthz.BeginRead(ctx)
	if err != nil {
		if errors.Is(err, authz.ErrUnauthenticated) {
			// No authorizable identity: hide the DB skill (fail hidden), don't error.
			return errSkillNotFound
		}
		return err
	}
	allowed, err := dec.AllowRead(ctx, rs.ID, rs.Scope, rs.UserID, rs.AgentID)
	if err != nil {
		return err
	}
	if !allowed {
		return errSkillNotFound
	}
	return nil
}

// viewContext builds the exact Skill visibility context for this invocation.
func (t *Tool) viewContext(ctx context.Context) ViewContext {
	return ViewContext{
		UserID:            authz.UserIDFromContext(ctx),
		AgentID:           authz.AgentIDFromContext(ctx),
		DisabledSkillRefs: t.disabledSkillRefs,
	}
}

// Action is one generated skill tool bound to the runner's skill Tool. Every
// action shares that one Tool: it holds the active sandbox Session and the
// projection lock, which belong to the Session rather than to a call.
type Action struct {
	spec SkillActionTool
	tool *Tool
}

// NewAction builds one skill action tool over the runner's skill Tool.
func NewAction(tool *Tool, spec SkillActionTool) *Action { return &Action{spec: spec, tool: tool} }

// RuntimeActionTools is the existing sandbox-read surface. Managed Skill CRUD
// is declared in Phase 1 but remains unregistered until Phase 3 moves its
// application service out of the HTTP adapter.
func RuntimeActionTools() []SkillActionTool {
	var out []SkillActionTool
	for _, spec := range SkillActionTools() {
		if spec.Name == "skill_load" || spec.Name == "skill_installed_search" {
			out = append(out, spec)
		}
	}
	return out
}

func (a *Action) Definition() tools.Definition { return a.spec.Definition("") }

func (a *Action) Execute(ctx context.Context, args map[string]any) (string, error) {
	if a == nil || a.tool == nil {
		return "", errors.New("skill tools are unavailable — no active sandbox Session")
	}
	out, err := SkillDispatch(ctx, a.tool, a.spec.Action, args)
	if err != nil {
		return "", err
	}
	text, ok := out.(string)
	if !ok {
		return tools.MarshalResult(out)
	}
	return text, nil
}

// Load reads one installed skill's current revision and projects it into the
// active sandbox Session. Identity comes from the context; the read PEP runs
// before any Home content is opened.
func (t *Tool) Load(ctx context.Context, in SkillLoadInput) (any, error) {
	return t.loadManagedOrImmutable(ctx, in.Name, in.Path, t.viewContext(ctx))
}

func resolvedIdentity(rs ResolvedSkill) Skill {
	return Skill{ID: rs.ID, Scope: rs.Scope, UserID: rs.UserID, AgentID: rs.AgentID, Name: rs.Name}
}

func (t *Tool) identityMerged(ctx context.Context, vc ViewContext) ([]ResolvedSkill, error) {
	rows, err := listManagedIdentitiesWhenAvailable(ctx, t.runtime, ViewContext{UserID: vc.UserID, AgentID: vc.AgentID})
	if err != nil {
		return nil, err
	}
	merged := t.svc.ListMerged(rows, t.projectSnapshot)
	return filterDisabled(merged, vc.DisabledSkillRefs), nil
}

func (t *Tool) hydrateAuthorized(ctx context.Context, merged []ResolvedSkill) ([]ResolvedSkill, error) {
	authorized, err := t.authorizeReadable(ctx, merged)
	if err != nil {
		return nil, err
	}
	out := make([]ResolvedSkill, 0, len(authorized))
	for _, rs := range authorized {
		if !isDBSkill(rs) {
			out = append(out, rs)
			continue
		}
		revision, err := t.runtime.LoadCurrentRevision(ctx, resolvedIdentity(rs))
		if errors.Is(err, errCurrentSkillSelectorMissing) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !sameSkillIdentity(resolvedIdentity(rs), revision.Skill) {
			return nil, ErrInvalidSkillRevision
		}
		if !invocationVisible(revision.Skill) {
			continue
		}
		rs.Skill = revision.Skill
		out = append(out, rs)
	}
	return out, nil
}

func (t *Tool) loadManagedOrImmutable(ctx context.Context, name, filename string, vc ViewContext) (string, error) {
	if filename == "" {
		filename = MainFile
	}
	if err := validateSkillPath(filename); err != nil {
		return "", err
	}
	merged, err := t.identityMerged(ctx, vc)
	if err != nil {
		return "", fmt.Errorf("resolve skill %q: %w", name, err)
	}
	if builtinName, ok := t.svc.builtinNameForReference(name); ok {
		name = builtinName
	}
	var resolved *ResolvedSkill
	for i := range merged {
		if merged[i].Name == name {
			resolved = &merged[i]
			break
		}
	}
	if resolved == nil {
		return "", fmt.Errorf("skill %q not found", name)
	}
	if len(filterVisibleResolvedSkills([]ResolvedSkill{*resolved}, pkgplugins.SystemPromptContext{
		RegisteredPluginIDs: t.registeredPluginIDs,
		EnabledPluginIDs:    t.enabledPluginIDs,
	})) == 0 {
		return "", errSkillNotFound
	}
	if err := t.authorizeLoadable(ctx, resolved); err != nil {
		return "", err
	}

	var data string
	var projection immutableSkillProjection
	if !isDBSkill(*resolved) {
		data, err = resolved.LoadImmutableFile(filename)
		if err != nil {
			return "", fmt.Errorf("load %s skill %q file %q: %w", resolved.Scope, name, filename, err)
		}
		projection, err = resolved.immutableProjection()
		if err != nil {
			return "", fmt.Errorf("prepare %s skill %q projection: %w", resolved.Scope, name, err)
		}
	} else {
		revision, readErr := t.runtime.LoadCurrentRevision(ctx, resolvedIdentity(*resolved))
		if readErr != nil {
			return "", fmt.Errorf("load skill %q: %w", name, readErr)
		}
		if !sameSkillIdentity(resolvedIdentity(*resolved), revision.Skill) || !validSkillDigest(revision.Skill.ContentDigest) {
			return "", ErrInvalidSkillRevision
		}
		if !invocationVisible(revision.Skill) {
			return "", errSkillNotFound
		}
		dataBytes, ok := revision.Files[filename]
		if !ok {
			return "", fmt.Errorf("load skill %q file %q: %w", name, filename, fs.ErrNotExist)
		}
		resolved.Skill = revision.Skill
		if err := t.touchReflectSkillRuntimeUse(ctx, resolved, vc); err != nil {
			return "", err
		}
		projection, err = managedSkillProjection(revision)
		if err != nil {
			return "", fmt.Errorf("prepare skill %q projection: %w", name, err)
		}
		data = string(dataBytes)
	}
	skillDir, err := t.projectSkill(projection)
	if err != nil {
		return "", fmt.Errorf("project skill %q: %w", name, err)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "<skill_dir>%s</skill_dir>\n", skillDir)
	fmt.Fprintf(&out, "<skill_content name=%q path=%q>\n%s\n</skill_content>", name, filename, data)
	return out.String(), nil
}

func managedSkillProjection(revision ManagedRevision) (immutableSkillProjection, error) {
	if !validInventoryComponent(revision.Skill.Scope) || !validInventoryComponent(revision.Skill.ID) || !validSkillDigest(revision.Skill.ContentDigest) {
		return immutableSkillProjection{}, ErrInvalidSkillRevision
	}
	if len(revision.Modes) != len(revision.Files) {
		return immutableSkillProjection{}, ErrInvalidSkillRevision
	}
	files := make([]revisionFile, 0, len(revision.Files))
	for name, content := range revision.Files {
		mode, ok := revision.Modes[name]
		if !ok || mode&fs.ModeType != 0 {
			return immutableSkillProjection{}, ErrInvalidSkillRevision
		}
		mode = mode.Perm() & 0o555
		if mode&0o444 == 0 {
			return immutableSkillProjection{}, ErrInvalidSkillRevision
		}
		files = append(files, revisionFile{Path: name, Mode: mode, Content: content})
	}
	files, err := validateRevisionFiles(files)
	if err != nil {
		return immutableSkillProjection{}, err
	}
	projected := make([]immutableSkillFile, 0, len(files))
	for _, file := range files {
		projected = append(projected, immutableSkillFile{path: file.Path, content: file.Content, mode: file.Mode})
	}
	return immutableSkillProjection{
		kind:   revision.Skill.Scope,
		id:     revision.Skill.ID,
		digest: revision.Skill.ContentDigest,
		files:  projected,
	}, nil
}

func (t *Tool) projectSkill(projection immutableSkillProjection) (string, error) {
	// One Tool belongs to one active Session. Serialize publication so concurrent
	// loads cannot replace a digest path while another publication is deciding
	// its exact Session view.
	t.projectionMu.Lock()
	defer t.projectionMu.Unlock()

	if t.session == nil {
		return "", errors.New("active sandbox Session is required")
	}
	if !validInventoryComponent(projection.kind) || !validInventoryComponent(projection.id) || !validSkillDigest(projection.digest) || len(projection.files) == 0 {
		return "", ErrInvalidSkillRevision
	}
	relative := path.Join("stella-skills", projection.kind, projection.id, projection.digest)
	projected := make([]pkgsandbox.ProjectedFile, 0, len(projection.files))
	for _, file := range projection.files {
		projected = append(projected, pkgsandbox.ProjectedFile{Path: file.path, Content: file.content, Mode: file.mode})
	}
	// This Session-private convenience copy is atomically published and verified
	// on every load, but it is not an isolation boundary from same-UID commands.
	// A concurrent command can race that verification or modify the copy later;
	// any mismatch the next load observes fails closed instead of replacing it.
	visible, err := t.session.Files().ProjectTempFiles(relative, projected)
	if err != nil {
		if errors.Is(err, pkgsandbox.ErrProjectionConflict) {
			return "", errors.Join(ErrInvalidSkillRevision, err)
		}
		return "", err
	}
	if !path.IsAbs(visible) || path.Clean(visible) != visible {
		return "", errors.New("sandbox Session returned an invalid projection path")
	}
	return visible, nil
}

func (t *Tool) touchReflectSkillRuntimeUse(ctx context.Context, resolved *ResolvedSkill, vc ViewContext) error {
	if vc.UserID == "" || vc.AgentID == "" {
		return nil
	}
	if resolved == nil || resolved.Scope != "user_agent" || resolved.UserID != vc.UserID || resolved.AgentID != vc.AgentID {
		return nil
	}
	if !IsReflectOwned(Skill{Metadata: resolved.Metadata}) {
		return nil
	}
	touchCtx, cancel := context.WithTimeout(ctx, runtimeUsageTouchTimeout)
	defer cancel()
	err := t.runtime.TouchReflectSkillRuntimeUseDigest(touchCtx, resolved.ID, vc.UserID, vc.AgentID, resolved.ContentDigest)
	if err != nil {
		return fmt.Errorf("claim runtime use for skill %q: %w", resolved.Name, err)
	}
	return nil
}
