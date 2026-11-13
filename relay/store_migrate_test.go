package relay_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-meshrelay/relay"
)

// oldClientsSchema turns a relay.db's clients table back into the one
// meshrelay v0.14.0 and earlier created, before it gained columns.
const oldClientsSchema = `
CREATE TABLE clients_old (
	name         TEXT PRIMARY KEY,
	role         TEXT NOT NULL,
	token_sha256 TEXT NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL
);
INSERT INTO clients_old SELECT name, role, token_sha256, created_at FROM clients;
DROP TABLE clients;
ALTER TABLE clients_old RENAME TO clients;
`

// oldRelayDB makes a relay.db as v0.14.0 left it, with one client.
func oldRelayDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.db")
	st, err := relay.OpenStore(context.Background(), path, time.Second)
	require.NoError(t, err)
	require.NoError(t, st.CreateClient(relay.ClientRecord{Name: "dev", Role: relay.RoleOperator, CreatedAt: time.UnixMilli(1000).UTC()}, "hd"))
	require.NoError(t, st.Close())
	db, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(oldClientsSchema)
	require.NoError(t, err)
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM pragma_table_info('clients') WHERE name = 'gateway'`).Scan(&n))
	require.Zero(t, n, "the fixture should have the old schema")
	return path
}

// A relay.db from before the clients table gained columns: snapshots read
// it as it is, and opening it adds the columns and keeps the rows.
func TestOldClientsTable(t *testing.T) {
	path := oldRelayDB(t)

	snap, err := relay.ReadSnapshot(context.Background(), path, relay.SnapshotOptions{})
	require.NoError(t, err)
	require.Len(t, snap.Clients, 1)
	assert.Equal(t, "dev", snap.Clients[0].Name)
	assert.Equal(t, "hd", snap.Clients[0].TokenHash)
	assert.Empty(t, snap.Clients[0].Gateway)

	for range 2 { // opening again changes nothing
		st, err := relay.OpenStore(context.Background(), path, time.Second)
		require.NoError(t, err)
		c, err := st.ClientByToken("hd")
		require.NoError(t, err)
		assert.Equal(t, relay.RoleOperator, c.Role)
		assert.True(t, c.CreatedAt.Equal(time.UnixMilli(1000)))
		assert.Empty(t, c.Gateway)
		require.NoError(t, st.Close())
	}

	st, err := relay.OpenStore(context.Background(), path, time.Second)
	require.NoError(t, err)
	require.NoError(t, st.CreateClient(relay.ClientRecord{Name: "cabin.laptop", Role: "host", CreatedAt: time.Now(),
		Gateway: "cabin", Note: "n"}, "hh"))
	require.NoError(t, st.Close())

	snap, err = relay.ReadSnapshot(context.Background(), path, relay.SnapshotOptions{})
	require.NoError(t, err)
	require.Len(t, snap.Clients, 2)
	assert.Equal(t, "cabin", snap.Clients[0].Gateway, "snapshots carry the new columns")
	assert.Equal(t, "hh", snap.Clients[0].TokenHash)
}
