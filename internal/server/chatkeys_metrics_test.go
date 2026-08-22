package server

import (
	"context"
	"errors"
	"testing"

	"voicx/internal/metrics"
)

type rotateFailureStore struct {
	ScopeKeyStore
	err error
}

func (s rotateFailureStore) RotateScopeKey(_ context.Context, _ int64, _ uint32, _ []byte, _ uint16) error {
	return s.err
}

func TestChatKeyFailuresAreCountedOnceAtPublicBoundaries(t *testing.T) {
	_, store, kek := testKeyManager(t, nil)
	m := metrics.New()
	manager := newChatKeyManager(
		rotateFailureStore{ScopeKeyStore: store, err: errors.New("rotate store failure")},
		kek,
		testLogger(),
		m.IncChatCryptoFailure,
	)
	ctx := t.Context()
	gen, _, err := manager.EnsureScope(ctx, 44)
	if err != nil {
		t.Fatalf("EnsureScope: %v", err)
	}
	if _, err := manager.open(ctx, 44, gen, "not-base64"); err == nil {
		t.Fatal("corrupt ciphertext decrypted")
	}
	if _, err := manager.open(ctx, 44, gen+1, "not-base64"); err == nil {
		t.Fatal("stale key generation decrypted")
	}
	if _, err := manager.sealFor(ctx, 44, gen, "not-a-public-key"); err == nil {
		t.Fatal("invalid public key was accepted")
	}
	if _, _, err := manager.rotate(ctx, 44); err == nil {
		t.Fatal("rotate succeeded despite store failure")
	}
	unconfigured := newChatKeyManager(nil, nil, testLogger(), m.IncChatCryptoFailure)
	if _, _, err := unconfigured.EnsureScope(ctx, 45); err == nil {
		t.Fatal("unconfigured key manager minted a scope key")
	}
	m.IncChatCryptoFailure("attacker-controlled-operation")

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var familyFound bool
	values := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "voicx_chat_crypto_failures_total" {
			continue
		}
		familyFound = true
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "operation" {
					values[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	if !familyFound {
		t.Fatal("chat crypto failure metric was not gathered")
	}
	for operation, want := range map[string]float64{
		"decrypt":  2,
		"seal_key": 1,
		"rotate":   1,
		"ensure":   1,
		"unknown":  1,
	} {
		if got := values[operation]; got != want {
			t.Errorf("%s failures = %v, want %v", operation, got, want)
		}
	}
	if _, leaked := values["attacker-controlled-operation"]; leaked {
		t.Fatal("attacker-controlled operation leaked into metric labels")
	}
}
