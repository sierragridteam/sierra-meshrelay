package relay_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sierragridteam/sierra-meshrelay/relay"
	"github.com/sierragridteam/sierra-meshrelay/relay/storetest"
)

func TestSQLiteStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) relay.Storage {
		st, err := relay.OpenStore(context.Background(), filepath.Join(t.TempDir(), "relay.db"), time.Second)
		require.NoError(t, err)
		return st
	})
}
