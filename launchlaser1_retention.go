package main

// LL1-TEL-2: Launch Laser 1 play data is kept for 24 months (privacy text).
// purgeLaunchLaser1Old deletes launchlaser1_events and launchlaser1_crashes
// rows whose received_at (unix ms) is older than 24 months. It runs once at
// startup and then daily. It never touches manrocket_* or Cynthion tables.

import (
	"context"
	"log"
	"time"
)

const launchLaser1RetentionMonths = 24

// launchLaser1RetentionTables is the closed list of tables the purge may touch.
var launchLaser1RetentionTables = [...]string{"launchlaser1_events", "launchlaser1_crashes"}

func launchLaser1RetentionCutoff(now time.Time) int64 {
	return now.UTC().AddDate(0, -launchLaser1RetentionMonths, 0).UnixMilli()
}

// purgeLaunchLaser1Old returns rows deleted per table.
func (s *Store) purgeLaunchLaser1Old(ctx context.Context, now time.Time) (map[string]int64, error) {
	cutoff := launchLaser1RetentionCutoff(now)
	out := make(map[string]int64, len(launchLaser1RetentionTables))
	for _, t := range launchLaser1RetentionTables {
		res, err := s.db.ExecContext(ctx, "DELETE FROM "+t+" WHERE received_at < ?", cutoff)
		if err != nil {
			return out, err
		}
		n, _ := res.RowsAffected()
		out[t] = n
	}
	return out, nil
}

func (s *Store) runLaunchLaser1Retention(ctx context.Context) {
	n, err := s.purgeLaunchLaser1Old(ctx, time.Now())
	if err != nil {
		log.Printf("launchlaser1 retention purge failed: %v", err)
		return
	}
	log.Printf("launchlaser1 retention purge (%d months): deleted launchlaser1_events=%d launchlaser1_crashes=%d",
		launchLaser1RetentionMonths, n["launchlaser1_events"], n["launchlaser1_crashes"])
}

// startLaunchLaser1Retention purges now, then every 24 h until ctx is done.
func (s *Store) startLaunchLaser1Retention(ctx context.Context) {
	go func() {
		s.runLaunchLaser1Retention(ctx)
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.runLaunchLaser1Retention(ctx)
			}
		}
	}()
}
