package replay

import (
	"fmt"
	"time"

	consensusengine "github.com/Overclock-Validator/mithril/pkg/consensus"
)

func formatAlpenglowObserverStatus(snapshot consensusengine.Snapshot) (string, bool) {
	if snapshot.Mode != consensusengine.ModeAlpenglowObserver || snapshot.Alpenglow == nil {
		return "", false
	}
	ag := snapshot.Alpenglow
	status := fmt.Sprintf("cert_replay match/miss/pending=%d/%d/%d mature=%d pre_window=%d latest_cert=%d latest_finalized=%d latest_replay=%d",
		ag.CertificateReplayMatches,
		ag.CertificateReplayMismatches,
		ag.CertificateReplayPending,
		ag.CertificateReplayMaturePending,
		ag.CertificateReplayPreWindowPending,
		ag.LatestCertificateSlot,
		ag.LatestFinalizedSlot,
		ag.LatestReplayBlockSlot,
	)
	if snapshot.Receiver != nil {
		recv := snapshot.Receiver
		status = fmt.Sprintf("votor conn=%d streams=%d msgs=%d votes=%d certs=%d decode_errors=%d last_msg=%s | %s",
			recv.ConnectionsAccepted,
			recv.StreamsReceived,
			recv.MessagesDecoded,
			recv.VotesDecoded,
			recv.CertificatesDecoded,
			recv.DecodeErrors,
			alpenglowMessageAgeLabel(recv.LastMessageAt),
			status,
		)
	}
	return status, true
}

func alpenglowMessageAgeLabel(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String()
}
