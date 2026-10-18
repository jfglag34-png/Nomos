package cron

import (
	"testing"
	"time"
)

func TestIsValid(t *testing.T) {
	cases := []struct {
		expr string
		ok   bool
	}{
		{"* * * * *", true},
		{"0 9 * * *", true},
		{"*/5 * * * *", true},
		{"0-5 * * * *", true},
		{"0,15,30 * * * *", true},
		{"", false},
		{"* * *", false},
		{"a * * * *", false},
		{"* * * * * *", false},
	}
	for _, c := range cases {
		if got := IsValid(c.expr); got != c.ok {
			t.Errorf("IsValid(%q)=%v want %v", c.expr, got, c.ok)
		}
	}
}

func TestNext(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	// every minute
	n := Next("* * * * *", base)
	if n == nil || n.Equal(base) || n.Before(base) {
		t.Fatalf("Next * expected after base, got %v", n)
	}
	if n.Minute() != 1 || n.Hour() != 10 {
		t.Fatalf("expected 10:01 got %v", n)
	}
	// hourly at 0
	n = Next("0 * * * *", base)
	if n == nil || n.Minute() != 0 || n.Hour() != 11 {
		t.Fatalf("hourly: got %v", n)
	}
	// daily 09:00
	n = Next("0 9 * * *", base)
	if n == nil || n.Hour() != 9 || n.Day() != 19 {
		t.Fatalf("daily 9: got %v", n)
	}
	// 5min
	base2 := time.Date(2026, 10, 18, 10, 3, 0, 0, time.UTC)
	n = Next("*/5 * * * *", base2)
	if n == nil || n.Minute() != 5 {
		t.Fatalf("*/5: got %v", n)
	}
	// invalid
	if Next("bad", base) != nil {
		t.Error("bad should be nil")
	}
	if Next("* * * * a", base) != nil {
		t.Error("bad char should be nil")
	}
}

func TestMatches(t *testing.T) {
	parts := []string{"0", "9", "*", "*", "*"}
	ts := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	if !matches(ts, parts) {
		t.Error("should match 09:00")
	}
	ts2 := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	if matches(ts2, parts) {
		t.Error("should not match 10:00")
	}
	// dow: Sunday =0
	parts2 := []string{"*", "*", "*", "*", "0"}
	sun := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC) // 2026-10-18 is Sunday
	if sun.Weekday() != time.Sunday {
		t.Skipf("not sunday %v", sun.Weekday())
	}
	if !matches(sun, parts2) {
		t.Error("dow 0 should match sunday")
	}
}

func TestFieldMatches(t *testing.T) {
	if !fieldMatches(5, "*", 0, 59) {
		t.Error("*")
	}
	if !fieldMatches(0, "*/5", 0, 59) {
		t.Error("*/5 0")
	}
	if !fieldMatches(5, "*/5", 0, 59) {
		t.Error("*/5 5")
	}
	if fieldMatches(3, "*/5", 0, 59) {
		t.Error("*/5 3 should not")
	}
	if !fieldMatches(5, "1-10", 0, 59) {
		t.Error("range")
	}
	if fieldMatches(0, "1-10", 0, 59) {
		t.Error("range 0")
	}
	if !fieldMatches(30, "0,15,30", 0, 59) {
		t.Error("list")
	}
	if fieldMatches(20, "0,15,30", 0, 59) {
		t.Error("list 20")
	}
	if !fieldMatches(7, "7", 0, 59) {
		t.Error("exact")
	}
	if fieldMatches(8, "7", 0, 59) {
		t.Error("exact 8")
	}
	if fieldMatches(5, "*/0", 0, 59) {
		t.Error("*/0 should false")
	}
	if fieldMatches(5, "5/2", 0, 59) {
		t.Error("a/b should false")
	}
	if !fieldMatches(1, "1-1", 0, 59) {
		t.Error("1-1")
	}
}

func TestHuman(t *testing.T) {
	if Human("*/5 * * * *") != "каждые 5 мин" {
		t.Error("human 5min")
	}
	if Human("0 * * * *") != "каждый час" {
		t.Error("human hourly")
	}
	if Human("0 9 * * *") != "каждый день в 09:00" {
		t.Error("human daily")
	}
	if Human("0 9 * * 1") != "по понедельникам в 09:00" {
		t.Error("human mon")
	}
	if Human("1 2 3 4 5") != "1 2 3 4 5" {
		t.Error("human fallback")
	}
}

func TestValidateTimezone(t *testing.T) {
	if ValidateTimezone("") != nil {
		t.Error("empty ok")
	}
	if ValidateTimezone("Local") != nil {
		t.Error("Local ok")
	}
	if ValidateTimezone("UTC") != nil {
		t.Error("UTC ok")
	}
	if ValidateTimezone("local") != nil {
		t.Error("local ok")
	}
	if ValidateTimezone("Europe/Minsk") != nil {
		t.Error("Europe/Minsk should pass (contains /)")
	}
	if ValidateTimezone("Europe/Moscow") != nil {
		t.Error("Europe/Moscow")
	}
	if err := ValidateTimezone("bad tz!"); err == nil {
		t.Error("bad tz should error")
	}
}

func TestNextFromTruncation(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 45, 0, time.UTC)
	n := Next("* * * * *", base)
	if n == nil {
		t.Fatal("nil")
	}
	// Truncate to minute then +1min => 10:01 even though base has 45sec
	if n.Minute() != 1 {
		t.Fatalf("expected 10:01 got %v", n)
	}
}

func TestNextYearLimit(t *testing.T) {
	base := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	// impossible: 31 Feb
	n := Next("0 0 31 2 *", base)
	if n != nil {
		t.Fatalf("31 Feb should be nil (year scan exhausted), got %v", n)
	}
}
