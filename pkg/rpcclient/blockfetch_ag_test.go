package rpcclient

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeAlpenglowFooterCertsIncludesFinalCert(t *testing.T) {
	final := base64.StdEncoding.EncodeToString([]byte("final-cert-bytes"))
	skip := base64.StdEncoding.EncodeToString([]byte("skip"))
	notar := base64.StdEncoding.EncodeToString([]byte("notar"))
	footer := &AlpenglowFooterRPC{
		FinalCert:      &final,
		SkipRewardCert: &skip,
		NotarRewardCert: &notar,
	}

	gotSkip, gotNotar, gotFinal, err := DecodeAlpenglowFooterCerts(footer)
	require.NoError(t, err)
	require.Equal(t, []byte("final-cert-bytes"), gotFinal)
	require.Equal(t, []byte("skip"), gotSkip)
	require.Equal(t, []byte("notar"), gotNotar)
}
