package statsd

import (
	"strings"
	"testing"
	"time"

	mithrilmetrics "github.com/Overclock-Validator/mithril/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type replayHistogramState struct {
	count uint64
	sum   float64
}

func readReplayHistogram(
	t *testing.T,
	metric Metric,
	label string,
) replayHistogramState {
	t.Helper()
	observer, err := metricsCollection.histograms[metric].GetMetricWithLabelValues(label)
	require.NoError(t, err)
	dtoMetric := &dto.Metric{}
	require.NoError(t, observer.(prometheus.Metric).Write(dtoMetric))
	return replayHistogramState{
		count: dtoMetric.GetHistogram().GetSampleCount(),
		sum:   dtoMetric.GetHistogram().GetSampleSum(),
	}
}

func readReplayCounter(t *testing.T, metric Metric, label string) float64 {
	t.Helper()
	counter, err := metricsCollection.counters[metric].GetMetricWithLabelValues(label)
	require.NoError(t, err)
	dtoMetric := &dto.Metric{}
	require.NoError(t, counter.Write(dtoMetric))
	return dtoMetric.GetCounter().GetValue()
}

func TestSendBlockReplayDiagnosticMetricsUsesSecondsAndExactMappings(t *testing.T) {
	var replay mithrilmetrics.BlockReplay
	timingCases := []struct {
		metric   Metric
		label    string
		timing   *mithrilmetrics.Timing
		duration time.Duration
	}{
		{AlpenglowVoteRewards, "replay_block", &replay.AlpenglowVoteRewards, 11 * time.Millisecond},
		{VoteRewardValidatorPreparation, "replay_block", &replay.VoteRewardDetails.ValidatorPreparation, 12 * time.Millisecond},
		{VoteRewardSkipCertificateValidation, "replay_block", &replay.VoteRewardDetails.SkipCertificateValidation, 13 * time.Millisecond},
		{VoteRewardNotarCertificateValidation, "replay_block", &replay.VoteRewardDetails.NotarCertificateValidation, 14 * time.Millisecond},
		{VoteRewardFinalCertificateDecode, "replay_block", &replay.VoteRewardDetails.FinalCertificateDecode, 15 * time.Millisecond},
		{VoteRewardFinalCertificateValidation, "replay_block", &replay.VoteRewardDetails.FinalCertificateValidation, 16 * time.Millisecond},
		{VoteRewardStatePreparation, "replay_block", &replay.VoteRewardDetails.StatePreparation, 17 * time.Millisecond},
		{VoteRewardAccountMutation, "replay_block", &replay.VoteRewardDetails.AccountMutation, 18 * time.Millisecond},
		{TxUpdateAccountsDuration, "replay_tx_sum", &replay.TxUpdateAccounts, 19 * time.Millisecond},
		{TxPublishRecordWritableAcct, "replay_tx_sum", &replay.TxPublishRecordWritableAcct, 20 * time.Millisecond},
		{TxPublishTouchedAccountState, "replay_tx_sum", &replay.TxPublishTouchedAccountState, 21 * time.Millisecond},
		{TxPublishStakeVoteBookkeeping, "replay_tx_sum", &replay.TxPublishStakeVoteBookkeeping, 22 * time.Millisecond},
		{TxFailedUpdateAccounts, "replay_tx_sum", &replay.TxFailedUpdateAccounts, 23 * time.Millisecond},
		{TxFailedPublicationPreparation, "replay_tx_sum", &replay.TxFailedPublicationPreparation, 24 * time.Millisecond},
		{TxFailedPayerPublication, "replay_tx_sum", &replay.TxFailedPayerPublication, 25 * time.Millisecond},
		{TxFailedNoncePublication, "replay_tx_sum", &replay.TxFailedNoncePublication, 26 * time.Millisecond},
	}

	histogramBefore := make(map[Metric]replayHistogramState, len(timingCases))
	for _, testCase := range timingCases {
		require.True(t, strings.HasSuffix(testCase.metric.String(), "_duration_seconds"))
		assert.Equal(t, turbinePipelineDurationBuckets, MetricToBuckets[testCase.metric])
		histogramBefore[testCase.metric] = readReplayHistogram(t, testCase.metric, testCase.label)
		testCase.timing.AddTiming(testCase.duration)
	}

	replay.VoteRewardDetails.ValidatorCacheHits = 2
	replay.VoteRewardDetails.ValidatorCacheMisses = 3
	replay.VoteRewardDetails.RewardValidators = 4
	replay.VoteRewardDetails.FinalSigners = 5
	replay.VoteRewardDetails.VoteAccountsUpdated = 6
	replay.TxPublicationTouchedAccounts = 7
	replay.TxPublicationTouchedAccountBytes = 8
	counterCases := []struct {
		metric Metric
		label  string
		delta  float64
	}{
		{VoteRewardValidatorCacheHits, "replay_block", 2},
		{VoteRewardValidatorCacheMisses, "replay_block", 3},
		{VoteRewardValidators, "replay_block", 4},
		{VoteRewardFinalSigners, "replay_block", 5},
		{VoteRewardAccountsUpdated, "replay_block", 6},
		{TxPublicationTouchedAccounts, "replay_tx_sum", 7},
		{TxPublicationTouchedAccountBytes, "replay_tx_sum", 8},
	}
	counterBefore := make(map[Metric]float64, len(counterCases))
	for _, testCase := range counterCases {
		counterBefore[testCase.metric] = readReplayCounter(t, testCase.metric, testCase.label)
	}

	SendBlockReplayMetrics(replay)

	for _, testCase := range timingCases {
		after := readReplayHistogram(t, testCase.metric, testCase.label)
		before := histogramBefore[testCase.metric]
		assert.Equal(t, before.count+1, after.count, testCase.metric.String())
		assert.InDelta(t, testCase.duration.Seconds(), after.sum-before.sum, 1e-12, testCase.metric.String())
	}
	for _, testCase := range counterCases {
		after := readReplayCounter(t, testCase.metric, testCase.label)
		assert.Equal(t, counterBefore[testCase.metric]+testCase.delta, after, testCase.metric.String())
	}
}

func TestSendBlockReplayDiagnosticMetricsSkipsAbsentOptionalTimings(t *testing.T) {
	timingCases := []struct {
		metric Metric
		label  string
	}{
		{AlpenglowVoteRewards, "replay_block"},
		{VoteRewardValidatorPreparation, "replay_block"},
		{VoteRewardSkipCertificateValidation, "replay_block"},
		{VoteRewardNotarCertificateValidation, "replay_block"},
		{VoteRewardFinalCertificateDecode, "replay_block"},
		{VoteRewardFinalCertificateValidation, "replay_block"},
		{VoteRewardStatePreparation, "replay_block"},
		{VoteRewardAccountMutation, "replay_block"},
		{TxUpdateAccountsDuration, "replay_tx_sum"},
		{TxPublishRecordWritableAcct, "replay_tx_sum"},
		{TxPublishTouchedAccountState, "replay_tx_sum"},
		{TxPublishStakeVoteBookkeeping, "replay_tx_sum"},
		{TxFailedUpdateAccounts, "replay_tx_sum"},
		{TxFailedPublicationPreparation, "replay_tx_sum"},
		{TxFailedPayerPublication, "replay_tx_sum"},
		{TxFailedNoncePublication, "replay_tx_sum"},
	}
	before := make(map[Metric]replayHistogramState, len(timingCases))
	for _, testCase := range timingCases {
		before[testCase.metric] = readReplayHistogram(t, testCase.metric, testCase.label)
	}

	SendBlockReplayMetrics(mithrilmetrics.BlockReplay{})

	for _, testCase := range timingCases {
		assert.Equal(
			t,
			before[testCase.metric],
			readReplayHistogram(t, testCase.metric, testCase.label),
			testCase.metric.String(),
		)
	}
}
