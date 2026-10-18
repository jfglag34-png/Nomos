package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

func IsValid(cron string) bool {
	return Next(cron, time.Now()) != nil
}

func Next(cron string, from time.Time) *time.Time {
	parts := strings.Fields(cron)
	if len(parts) != 5 {
		return nil
	}
	// minimal validation: each field numeric/*/,-
	for _, p := range parts {
		if p == "" {
			return nil
		}
		for _, ch := range p {
			if !(ch >= '0' && ch <= '9' || ch == '*' || ch == '/' || ch == '-' || ch == ',') {
				return nil
			}
		}
	}
	// brute force next minute up to 1 year
	t := from.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 525600; i++ {
		if matches(t, parts) {
			n := t
			return &n
		}
		t = t.Add(time.Minute)
	}
	return nil
}

func matches(t time.Time, parts []string) bool {
	// parts: min hour dom month dow
	min := parts[0]
	hour := parts[1]
	dom := parts[2]
	mon := parts[3]
	dow := parts[4]
	if !fieldMatches(t.Minute(), min, 0, 59) {
		return false
	}
	if !fieldMatches(t.Hour(), hour, 0, 23) {
		return false
	}
	if !fieldMatches(t.Day(), dom, 1, 31) {
		return false
	}
	if !fieldMatches(int(t.Month()), mon, 1, 12) {
		return false
	}
	if !fieldMatches(int(t.Weekday()), dow, 0, 6) {
		return false
	}
	return true
}

func fieldMatches(val int, expr string, min, max int) bool {
	if expr == "*" {
		return true
	}
	// handle */n, a-b, a,b, */n etc simplified
	if strings.Contains(expr, ",") {
		for _, p := range strings.Split(expr, ",") {
			if fieldMatches(val, p, min, max) {
				return true
			}
		}
		return false
	}
	if strings.HasPrefix(expr, "*/") {
		n, err := strconv.Atoi(strings.TrimPrefix(expr, "*/"))
		if err != nil || n == 0 {
			return false
		}
		return val%n == 0
	}
	if strings.Contains(expr, "-") {
		ps := strings.SplitN(expr, "-", 2)
		a, _ := strconv.Atoi(ps[0])
		b, _ := strconv.Atoi(ps[1])
		return val >= a && val <= b
	}
	if strings.Contains(expr, "/") {
		return false
	}
	n, err := strconv.Atoi(expr)
	if err != nil {
		return false
	}
	_ = max
	_ = min
	return val == n
}

func Human(cron string) string {
	switch cron {
	case "*/5 * * * *":
		return "каждые 5 мин"
	case "0 * * * *":
		return "каждый час"
	case "0 9 * * *":
		return "каждый день в 09:00"
	case "0 9 * * 1":
		return "по понедельникам в 09:00"
	}
	return cron
}

func ValidateTimezone(tz string) error {
	if tz == "" || tz == "Local" || tz == "UTC" || tz == "local" {
		return nil
	}
	if strings.Contains(tz, "/") {
		return nil
	}
	if _, err := time.LoadLocation(tz); err == nil {
		return nil
	}
	return fmt.Errorf("неизвестная таймзона %q", tz)
}
