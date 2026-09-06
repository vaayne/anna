package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CherryHQ/stella/internal/authz"
)

// MutationFence holds the process-wide admission boundary while a plugin
// mutation is committed and its published runners are retired.
type MutationFence func(context.Context, func() error) error

var (
	ErrNestedMutation       = errors.New("plugin: nested mutation")
	ErrCommitOutcomeUnknown = errors.New("plugin: commit outcome unknown")
)

type mutationContextKey struct{}

// WithMutationTx runs one user-authorized plugin mutation under the process
// mutation fence. The callback receives a short-lived authority-bound Access
// and the raw transaction for trusted backends that must bind another service
// to this transaction. The callback must use the callback context passed as
// its first argument for nested plugin calls; the original context does not
// carry the transaction marker. The callback must not commit or roll back tx.
func (s *Service) WithMutationTx(ctx context.Context, authority authz.Authority, fn func(context.Context, *Access, pgx.Tx) error) error {
	if s == nil || s.db == nil || s.q == nil || s.mutationFence == nil || ctx == nil || fn == nil || !authority.Valid() || authority.Kind() != authz.ActorUser {
		return ErrForbidden
	}
	if mutationInProgress(ctx) || s.txBound {
		return ErrNestedMutation
	}
	return s.mutationFence(ctx, func() error {
		tx, err := s.db.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()

		bound := *s
		bound.q = s.q.WithTx(tx)
		bound.mutationTx = tx
		bound.txBound = true
		mutationCtx := context.WithValue(ctx, mutationContextKey{}, struct{}{})
		active := &atomic.Bool{}
		active.Store(true)
		access := &Access{service: &bound, authority: authority, active: active}

		var callbackErr error
		func() {
			defer active.Store(false)
			callbackErr = fn(mutationCtx, access, tx)
		}()
		if callbackErr != nil {
			return callbackErr
		}
		if err := tx.Commit(ctx); err != nil {
			return classifyCommitError(err)
		}
		return nil
	})
}

func mutationInProgress(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	_, ok := ctx.Value(mutationContextKey{}).(struct{})
	return ok
}

func classifyCommitError(err error) error {
	if err == nil || errors.Is(err, pgx.ErrTxCommitRollback) || pgconn.SafeToRetry(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrCommitOutcomeUnknown, err)
}
