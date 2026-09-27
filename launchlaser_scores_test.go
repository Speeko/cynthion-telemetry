package main

import (
	"testing"
	"time"
)

func TestPlausibleScore(t *testing.T) {
	cases := []struct {
		score, runMS, kills int64
		want                bool
	}{
		{42, 60_000, 30, true},
		{15, 3_000, 1, true}, // a Mothership kill early is still fine
		{0, 60_000, 0, false},
		{99_999, 1_000, 1, false},
		{500, 600_000, 5, false}, // 100 points a kill
		{launchLaserMaxScore + 1, 0, 0, false},
	}
	for _, c := range cases {
		if got := plausibleScore(c.score, c.runMS, c.kills); got != c.want {
			t.Errorf("plausibleScore(%d,%d,%d) = %v, want %v", c.score, c.runMS, c.kills, got, c.want)
		}
	}
}

func TestPeriodStart(t *testing.T) {
	now := time.Date(2026, 9, 26, 15, 4, 5, 0, time.UTC)
	if got := periodStart("day", now); got != time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("day start = %d", got)
	}
	if got := periodStart("week", now); got != now.Add(-7*24*time.Hour).UnixMilli() {
		t.Errorf("week start = %d", got)
	}
	if periodStart("all", now) != 0 {
		t.Error("all should be 0")
	}
}

func TestGameTag(t *testing.T) {
	if gameTag("launchlaser", "a1") != "d0b5" { // the game client derives the same tag
		t.Errorf("launchlaser tag = %s", gameTag("launchlaser", "a1"))
	}
	if gameTag("manrocket", "x") != installTag("x") {
		t.Error("manrocket tags must not change")
	}
}

func TestScoreGameModes(t *testing.T) {
	for _, g := range []string{"launchlaser", "launchlaser1"} {
		if !allowedLeaderboardGames[g] || !validLeaderboardMode(g, "score") || validLeaderboardMode(g, "level") {
			t.Errorf("%s should allow only mode=score", g)
		}
	}
	if validLeaderboardMode("manrocket", "score") {
		t.Error("manrocket has no score board")
	}
}
