package daemon

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/solana"
)

// walletEvery is how often the agent reads the node wallet's SOL balance. The node waits at start
// until it holds 0.005 SOL, which only its log says; the balance lets the console tell the owner.
const walletEvery = time.Minute

// MinSOL is the balance the node waits for at start (the node's NodeManager/node/balance/balanceHandler.ts).
const MinSOL = 0.005

type walletBalance struct {
	read func(ctx context.Context) (float64, error)

	mu   sync.Mutex
	last *api.WalletStatus
}

func newWalletBalance(network, address string) *walletBalance {
	url := solana.RPCURL(network)
	return &walletBalance{read: func(ctx context.Context) (float64, error) {
		return solana.Balance(ctx, url, address)
	}}
}

func (w *walletBalance) run(ctx context.Context) {
	for ctx.Err() == nil {
		w.refresh(ctx)
		sleep(ctx, walletEvery)
	}
}

// refresh reads the balance; a failed read keeps the last good reading next to the error.
func (w *walletBalance) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sol, err := w.read(ctx)

	w.mu.Lock()
	defer w.mu.Unlock()
	next := api.WalletStatus{}
	if w.last != nil {
		next.Sol, next.ReadAt = w.last.Sol, w.last.ReadAt
	}
	if err != nil {
		msg := err.Error()
		if w.last == nil || w.last.Error == nil || *w.last.Error != msg {
			slog.Warn("wallet balance", "err", err)
		}
		next.Error = &msg
	} else {
		if w.last != nil && w.last.Error != nil {
			slog.Info("wallet balance readable again", "sol", sol)
		}
		now := time.Now().UTC()
		next.Sol, next.ReadAt = &sol, &now
	}
	w.last = &next
}

func (w *walletBalance) status() *api.WalletStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// waitingForSOL reports that the last good reading is below MinSOL: the node waits at start and
// has nothing to lose from a restart.
func (w *walletBalance) waitingForSOL() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last != nil && w.last.Sol != nil && *w.last.Sol < MinSOL
}
