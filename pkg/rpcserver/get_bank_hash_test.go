package rpcserver

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/mr-tron/base58"
	"github.com/stretchr/testify/require"
)

func TestGetBankHashRejectsInexactSlots(t *testing.T) {
	server := &RpcServer{}
	for _, params := range [][]interface{}{
		{},
		{-1.0},
		{42.5},
		{float64(1 << 53)},
		{42.0, true},
	} {
		_, err := server.GetBankHash(context.Background(), mustRawParams(t, params))
		var invalid *InvalidParamsError
		require.ErrorAs(t, err, &invalid)
	}
}

func TestGetBankHashResolvesExactSlot(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "accounts"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "largest_file_id"), make([]byte, 8), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap_high_file_id"), make([]byte, 8), 0o644))
	db, err := accountsdb.OpenDb(dir)
	require.NoError(t, err)
	t.Cleanup(func() { db.CloseDb() })
	want := []byte{1, 2, 3}
	require.NoError(t, db.StoreBankHashForSlot(42, want))

	got, err := (&RpcServer{acctsDb: db}).GetBankHash(context.Background(), mustRawParams(t, []interface{}{float64(42)}))
	require.NoError(t, err)
	require.Equal(t, base58.Encode(want), got)
}
