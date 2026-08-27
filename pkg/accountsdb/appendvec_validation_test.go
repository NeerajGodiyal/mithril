package accountsdb

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppendVecAccountUnmarshalRejectsInvalidRecord(t *testing.T) {
	for _, test := range []struct {
		name   string
		record []byte
		err    error
	}{
		{name: "short header", record: make([]byte, hdrLen-1), err: io.ErrUnexpectedEOF},
		{name: "short data", record: func() []byte {
			record := make([]byte, hdrLen+1)
			binary.LittleEndian.PutUint64(record[dataLenOffset:], 2)
			return record
		}(), err: io.ErrUnexpectedEOF},
		{name: "oversized data", record: func() []byte {
			record := make([]byte, hdrLen)
			binary.LittleEndian.PutUint64(record[dataLenOffset:], maxAppendVecAccountDataLen+1)
			return record
		}()},
		{name: "invalid executable", record: func() []byte {
			record := make([]byte, hdrLen)
			record[96] = 2
			return record
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := new(AppendVecAccount).Unmarshal(bytes.NewReader(test.record))
			require.Error(t, err)
			if test.err != nil {
				require.ErrorIs(t, err, test.err)
			}
		})
	}
}
