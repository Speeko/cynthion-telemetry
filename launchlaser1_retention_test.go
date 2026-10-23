package main

import (
	"context"
	"testing"
	"time"
)

func TestLaunchLaser1RetentionPurgesOnlyOldLL1Rows(t *testing.T) {
	s, _ := newLL1TestServer(t, testLaunchLaser1Key)
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	cutoff := launchLaser1RetentionCutoff(now)
	if want := time.Date(2024, 10, 23, 4, 0, 0, 0, time.UTC).UnixMilli(); cutoff != want {
		t.Fatalf("cutoff=%d want %d (24 months, ms)", cutoff, want)
	}
	old := cutoff - 1 // 1 ms past 24 months
	edge := cutoff    // exactly 24 months: kept
	recent := now.UnixMilli() - 1000
	c := Crash{InstallID: "i", ErrorSummary: "boom"}
	for _, at := range []int64{old, edge, recent} {
		if _, err := s.store.insertLaunchLaser1Events(ctx, sampleBatch(2), at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.insertLaunchLaser1Crash(ctx, c, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.insertManRocketEvents(ctx, sampleBatch(2), at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.insertManRocketCrash(ctx, c, at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.insertEvents(ctx, sampleBatch(2), at); err != nil {
			t.Fatal(err)
		}
		if _, err := s.store.insertCrash(ctx, c, at); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.store.purgeLaunchLaser1Old(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n["launchlaser1_events"] != 2 || n["launchlaser1_crashes"] != 1 || len(n) != 2 {
		t.Fatalf("deleted=%v", n)
	}
	want := map[string]int{
		"launchlaser1_events": 4, "launchlaser1_crashes": 2,
		"manrocket_events": 6, "manrocket_crashes": 3,
		"events": 6, "crashes": 3,
	}
	for table, w := range want {
		if got := ll1Count(t, s, table); got != w {
			t.Fatalf("%s=%d want %d", table, got, w)
		}
	}
	var oldest int64
	if err := s.store.db.QueryRow(`SELECT MIN(received_at) FROM launchlaser1_events`).Scan(&oldest); err != nil || oldest != edge {
		t.Fatalf("oldest=%d err=%v", oldest, err)
	}
	// Second run is a no-op.
	if n, err := s.store.purgeLaunchLaser1Old(ctx, now); err != nil || n["launchlaser1_events"] != 0 || n["launchlaser1_crashes"] != 0 {
		t.Fatalf("rerun deleted=%v err=%v", n, err)
	}
}
