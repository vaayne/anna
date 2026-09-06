package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/internal/authz"
	"github.com/CherryHQ/stella/internal/plugin"
)

func TestUnifiedPluginWithMutationTxRollsBackCallbackError(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-tx-bind@example.test", false)
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	callbackRan := false
	callbackErr := errors.New("caller abort")
	err := service.WithMutationTx(ctx, user, func(_ context.Context, access *plugin.Access, _ pgx.Tx) error {
		callbackRan = true
		_, _, err := access.CreateCustom(ctx, transactionDefinition(), transactionConfig())
		if err != nil {
			return err
		}
		return callbackErr
	})
	if !callbackRan || !errors.Is(err, callbackErr) {
		t.Fatalf("callback result = ran:%v err:%v", callbackRan, err)
	}
	var definitions, configs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE namespace = 'tx_custom'`).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE namespace = 'tx_custom'`).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if definitions != 0 || configs != 0 {
		t.Fatalf("callback rollback left rows: definitions=%d configs=%d", definitions, configs)
	}

	var captured *plugin.Access
	if err := service.WithMutationTx(ctx, user, func(_ context.Context, access *plugin.Access, _ pgx.Tx) error {
		captured = access
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captured.ListDefinitions(ctx); !errors.Is(err, plugin.ErrForbidden) {
		t.Fatalf("captured Access after callback = %v, want forbidden", err)
	}
}

func TestUnifiedPluginWithMutationTxRollbackRestoresConfigAndPolicies(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-tx-delete@example.test", false)
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	access, err := service.Begin(user)
	if err != nil {
		t.Fatal(err)
	}
	definition, config, err := access.CreateCustom(ctx, plugin.Definition{
		Namespace: "tx_delete", DisplayName: "Transactional delete", Backend: plugin.BackendMCP,
		Spec: json.RawMessage(`{}`),
	}, plugin.Config{Scope: plugin.ScopeUser, Enabled: boolPtr(false)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO tool_override (scope, user_id, enabled, plugin_id, local_tool_name)
		VALUES ('user', $1, true, $2, 'tool')
	`, user.UserID(), definition.ID); err != nil {
		t.Fatal(err)
	}

	callbackErr := errors.New("outer operation failed")
	err = service.WithMutationTx(ctx, user, func(_ context.Context, bound *plugin.Access, _ pgx.Tx) error {
		if err := bound.DeleteConfig(ctx, definition.ID, config.ID, config.Revision); err != nil {
			return err
		}
		if err := bound.DeleteDefinition(ctx, definition.ID, definition.Revision); err != nil {
			return err
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("bound delete callback = %v", err)
	}
	var policies, definitions, configs int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM tool_override WHERE plugin_id = $1`, definition.ID).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_definition WHERE id = $1`, definition.ID).Scan(&definitions); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plugin_config WHERE id = $1`, config.ID).Scan(&configs); err != nil {
		t.Fatal(err)
	}
	if policies != 1 || definitions != 1 || configs != 1 {
		t.Fatalf("outer rollback did not restore rows: policies=%d definitions=%d configs=%d", policies, definitions, configs)
	}
}

func TestUnifiedPluginWithMutationTxRejectsNonUserWithoutCallback(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	agent, err := authz.NewAgentAuthority("owner", "agent")
	if err != nil {
		t.Fatal(err)
	}
	callbackRan := false
	err = service.WithMutationTx(ctx, agent, func(_ context.Context, _ *plugin.Access, _ pgx.Tx) error {
		callbackRan = true
		return nil
	})
	if !errors.Is(err, plugin.ErrForbidden) || callbackRan {
		t.Fatalf("unauthorized binder = err:%v callback:%v", err, callbackRan)
	}
}

func TestUnifiedPluginWithMutationTxInvalidatesAccessOnPanic(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-tx-panic@example.test", false)
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, inlinePluginMutationFence)
	var captured *plugin.Access
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("callback panic was swallowed")
			}
		}()
		_ = service.WithMutationTx(ctx, user, func(_ context.Context, access *plugin.Access, _ pgx.Tx) error {
			captured = access
			panic("callback panic")
		})
	}()
	if _, err := captured.ListDefinitions(ctx); !errors.Is(err, plugin.ErrForbidden) {
		t.Fatalf("captured Access after panic = %v, want forbidden", err)
	}
}

func TestUnifiedPluginWithMutationTxRejectsNestedUsingProvidedContext(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-tx-nested@example.test", false)
	fenceCalls := 0
	fence := func(_ context.Context, mutate func() error) error {
		fenceCalls++
		return mutate()
	}
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, fence)
	callbackRan := false
	err := service.WithMutationTx(ctx, user, func(mutationCtx context.Context, _ *plugin.Access, _ pgx.Tx) error {
		nestedErr := service.WithMutationTx(mutationCtx, user, func(context.Context, *plugin.Access, pgx.Tx) error {
			callbackRan = true
			return nil
		})
		if !errors.Is(nestedErr, plugin.ErrNestedMutation) {
			return nestedErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("outer mutation = %v", err)
	}
	if callbackRan || fenceCalls != 1 {
		t.Fatalf("nested mutation callback=%v fence calls=%d, want false/1", callbackRan, fenceCalls)
	}
}

func TestUnifiedPluginWithMutationTxExpiresAccessBeforeFenceUnlock(t *testing.T) {
	db := newTestDB(t)
	ctx := t.Context()
	user := insertPluginUser(t, db, "plugin-tx-expire@example.test", false)
	var captured *plugin.Access
	fence := func(ctx context.Context, mutate func() error) error {
		if err := mutate(); err != nil {
			return err
		}
		if _, err := captured.ListDefinitions(ctx); !errors.Is(err, plugin.ErrForbidden) {
			return fmt.Errorf("access remained usable while fence held: %w", err)
		}
		return nil
	}
	service := plugin.NewService(db, nil, plugin.NewCatalog(), plugin.BackendPolicy{Validate: noopPluginValidator, Transition: noopBackendTransition}, fence)
	if err := service.WithMutationTx(ctx, user, func(_ context.Context, access *plugin.Access, _ pgx.Tx) error {
		captured = access
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func transactionDefinition() plugin.Definition {
	return plugin.Definition{
		Namespace: "tx_custom", DisplayName: "Transactional custom", Backend: plugin.BackendMCP,
		Spec: json.RawMessage(`{}`),
	}
}

func transactionConfig() plugin.Config {
	return plugin.Config{Scope: plugin.ScopeUser, Enabled: boolPtr(false)}
}
