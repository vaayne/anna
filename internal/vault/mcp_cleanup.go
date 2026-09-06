package vault

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/CherryHQ/stella/pkg/db/sqlc"
)

// DeleteMCPConfigCredentialsTx removes only one config's reserved credential
// family. The trusted caller must first authorize and lock that config, then
// perform the config mutation in this same transaction. No key is needed:
// cleanup must also work after a deployment removes its encryption key.
// This capability is deliberately absent from public Vault Access.
func DeleteMCPConfigCredentialsTx(ctx context.Context, tx pgx.Tx, configID uuid.UUID) error {
	if tx == nil || configID == uuid.Nil {
		return errors.New("vault: MCP credential cleanup requires a transaction and config UUID")
	}
	return sqlc.New(tx).DeleteMCPConfigCredentials(ctx, configID.String())
}
