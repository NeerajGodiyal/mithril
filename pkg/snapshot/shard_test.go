package snapshot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Overclock-Validator/mithril/pkg/accountsdb"
	"github.com/cockroachdb/pebble"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestNewShardLoggerWithErrorClosesPartialSetup(t *testing.T) {
	logsDir := t.TempDir()
	want := errors.New("create failed")
	var first *os.File
	calls := 0
	_, err := newShardLogger(2, logsDir, func(path string) (*os.File, error) {
		calls++
		if calls == 2 {
			return nil, want
		}
		var err error
		first, err = os.Create(path)
		return first, err
	})
	require.ErrorIs(t, err, want)
	require.Error(t, first.Sync(), "the first shard file must be closed after later setup fails")
}

func TestShardLoggerConcurrentCloseDoesNotRaceSend(t *testing.T) {
	logger, err := NewShardLoggerWithError(1, t.TempDir())
	require.NoError(t, err)

	start := make(chan struct{})
	var sends sync.WaitGroup
	for i := byte(1); i <= 64; i++ {
		sends.Add(1)
		go func(i byte) {
			defer sends.Done()
			<-start
			logger.EnqueueRequest(solana.PublicKey{i}, accountsdb.AccountIndexEntry{Slot: uint64(i)})
		}(i)
	}
	close(start)
	closeErr := make(chan error, 1)
	go func() { closeErr <- logger.Close(context.Background()) }()
	sends.Wait()
	require.NoError(t, <-closeErr)
}

func TestShardLoggerRemovesRawLogAfterFlush(t *testing.T) {
	logsDir := t.TempDir()
	logger, err := NewShardLoggerWithError(1, logsDir)
	require.NoError(t, err)
	logger.EnqueueRequest(solana.PublicKey{1}, accountsdb.AccountIndexEntry{Slot: 1})
	require.NoError(t, logger.Close(context.Background()))

	_, err = os.Stat(filepath.Join(logsDir, "000"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(logsDir, "000.sst"))
	require.NoError(t, err)
}

func TestIngestSSTFilesClosesPebbleOnFailure(t *testing.T) {
	indexDir := filepath.Join(t.TempDir(), "index")
	logsDir := filepath.Join(t.TempDir(), "logs")
	require.NoError(t, os.Mkdir(logsDir, 0o755))

	_, err := ingestSSTFiles(indexDir, logsDir)
	require.ErrorContains(t, err, "0 SST files")

	db, err := pebble.Open(indexDir, accountsdb.NewAccountsIndexPebbleOptions(nil))
	require.NoError(t, err, "failed ingest must release Pebble's directory lock")
	require.NoError(t, db.Close())
}
