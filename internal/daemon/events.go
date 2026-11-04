package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/nosana-ci/host-agent/internal/api"
	"github.com/nosana-ci/host-agent/internal/control"
	"github.com/nosana-ci/host-agent/internal/store"
)

// eventBatch is host-manager's limit on events per request.
const eventBatch = 50

// eventLoop sends the recorded events to host-manager while the host is linked: right after one is
// recorded, and again every heartbeatEvery while some wait. Failures back off up to five minutes;
// the events stay in the state meanwhile.
func (d *daemon) eventLoop(ctx context.Context) {
	fails := 0
	for ctx.Err() == nil {
		wait := heartbeatEvery
		if err := d.sendEvents(ctx); err != nil {
			fails++
			wait = min(heartbeatEvery<<min(fails, 5), 5*time.Minute)
			if ctx.Err() == nil {
				slog.Warn("events", "err", err, "retryIn", wait)
			}
		} else {
			fails = 0
		}
		select {
		case <-ctx.Done():
		case <-d.st.EventsAdded():
		case <-time.After(wait):
		}
	}
}

// sendEvents sends the waiting events in batches. A batch host-manager refuses as invalid is
// dropped, so one bad event cannot hold up the rest.
func (d *daemon) sendEvents(ctx context.Context) error {
	for {
		state := d.st.State()
		if !state.Claimed || len(state.Events) == 0 {
			return nil
		}
		batch := state.Events[:min(len(state.Events), eventBatch)]
		err := d.ctrl.Events(ctx, toAPIEvents(batch))
		var he *control.HTTPError
		if errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusUnprocessableEntity) {
			slog.Warn("events refused; dropping them", "count", len(batch), "err", err)
			err = nil
		}
		if err != nil {
			return err
		}
		ids := make([]string, len(batch))
		for i, e := range batch {
			ids[i] = e.ID
		}
		if err := d.st.Delivered(ids); err != nil {
			return err
		}
	}
}

func toAPIEvents(events []store.Event) []api.AgentEvent {
	out := make([]api.AgentEvent, len(events))
	for i, e := range events {
		out[i] = api.AgentEvent{Id: e.ID, At: e.At, Type: e.Type, Level: api.AgentEventLevel(e.Level), Message: e.Message}
		if e.Verb != "" {
			out[i].Verb = &e.Verb
		}
	}
	return out
}
