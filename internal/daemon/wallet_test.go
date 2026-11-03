package daemon

import (
	"context"
	"errors"
	"testing"
)

func TestWalletBalanceKeepsLastReadingOnError(t *testing.T) {
	var sol float64
	var err error
	w := &walletBalance{read: func(context.Context) (float64, error) { return sol, err }}
	if w.status() != nil {
		t.Fatal("status before the first read should be nil")
	}

	sol = 0.002
	w.refresh(context.Background())
	s := w.status()
	if s.Sol == nil || *s.Sol != 0.002 || s.ReadAt == nil || s.Error != nil {
		t.Fatalf("after a good read: %+v", s)
	}

	err = errors.New("rpc down")
	w.refresh(context.Background())
	s = w.status()
	if s.Sol == nil || *s.Sol != 0.002 || s.ReadAt == nil || s.Error == nil || *s.Error != "rpc down" {
		t.Fatalf("after a failed read: %+v", s)
	}

	sol, err = 0.01, nil
	w.refresh(context.Background())
	if s = w.status(); *s.Sol != 0.01 || s.Error != nil {
		t.Fatalf("after recovering: %+v", s)
	}
}
