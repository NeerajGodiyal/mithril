package rewardcerts

import (
	"context"
	"fmt"
	"strings"

	"github.com/Overclock-Validator/mithril/pkg/alpenglow"
	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

// VotorListener ingests Alpenglow Votor QUIC messages and feeds skip/notar
// votes into a reward certificate builder.
type VotorListener struct {
	builder  *Builder
	receiver *alpenglow.Receiver
}

// StartVotorListener listens for Votor QUIC messages on bindAddr and records
// skip/notar votes in builder.
func StartVotorListener(ctx context.Context, builder *Builder, bindAddr string, maxMessageBytes int64) (*VotorListener, error) {
	if builder == nil {
		return nil, fmt.Errorf("rewardcerts: nil builder")
	}
	bindAddr = strings.TrimSpace(bindAddr)
	if bindAddr == "" {
		return nil, fmt.Errorf("rewardcerts: empty Votor bind address")
	}

	cfg := alpenglow.DefaultReceiverConfig()
	cfg.BindAddr = bindAddr
	if maxMessageBytes > 0 {
		cfg.MaxMessageBytes = maxMessageBytes
	}
	cfg.OnMessage = builder.ObserveVotorMessage

	receiver, err := alpenglow.NewReceiver(cfg, nil)
	if err != nil {
		return nil, err
	}

	listener := &VotorListener{
		builder:  builder,
		receiver: receiver,
	}
	go func() {
		if err := receiver.Run(ctx); err != nil && ctx.Err() == nil {
			mlog.Log.Warnf("block production Votor listener stopped: %v", err)
		}
	}()
	mlog.Log.Infof("block production Votor listener on %s (reward certs for slot N-%d)", receiver.Addr(), SlotsForReward)
	return listener, nil
}

func (l *VotorListener) Close() error {
	if l == nil || l.receiver == nil {
		return nil
	}
	return l.receiver.Close()
}

// ObserveVotorMessage records skip/notar votes from a decoded Votor message.
func (b *Builder) ObserveVotorMessage(msg alpenglow.Message) {
	if msg.Vote == nil {
		return
	}
	switch msg.Vote.Vote.Type {
	case alpenglow.VoteTypeSkip, alpenglow.VoteTypeNotarize:
		b.AddVote(*msg.Vote)
	}
}
