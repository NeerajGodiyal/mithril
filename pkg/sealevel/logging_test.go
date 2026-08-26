package sealevel

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLogRecorderBoundsRecordedBytes(t *testing.T) {
	recorder := LogRecorder{BytesLimit: 5}
	recorder.Log("1234")
	recorder.Log("5")
	recorder.Log("")
	recorder.Log("ignored")

	require.Equal(t, []string{"1234", "5", "", "Log truncated"}, recorder.Logs)
	require.Equal(t, uint64(5), recorder.BytesWritten)
	require.True(t, recorder.Truncated)
}
