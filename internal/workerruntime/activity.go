package workerruntime

import (
	"context"
	"log"
	"time"

	"github.com/hollis-labs/tether/internal/workerwire"
)

func (w *Worker) activityLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		scanCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		batch, err := w.Activity(scanCtx, time.Now())
		cancel()
		if err != nil {
			log.Printf("worker activity scan: %v", err)
		} else if ctx.Err() == nil {
			if err := w.Send(workerwire.Frame{T: "activity", Activity: batch}); err != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
