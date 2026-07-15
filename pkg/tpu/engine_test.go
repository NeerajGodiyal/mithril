package tpu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func TestTPUStartStop(t *testing.T) {
	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	svc, err := Start(context.Background(), Config{
		Name:       "test-tpu",
		ListenAddr: "127.0.0.1:0",
		Identity:   identity,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	addr, err := svc.AdvertisedQUICAddr()
	if err != nil {
		t.Fatalf("AdvertisedQUICAddr: %v", err)
	}
	if addr.Port == 0 {
		t.Fatalf("expected ephemeral TPU port")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
