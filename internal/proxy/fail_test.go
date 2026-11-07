package proxy

import (
	"testing"
	"time"
)

func TestFailTracker(t *testing.T) {
	now := time.Unix(1000, 0)
	f := newFailTracker(func() time.Time { return now }, time.Minute)

	if f.blocked("2") {
		t.Fatal("a fresh key must not be blocked")
	}
	f.cooldown("2")
	if !f.blocked("2") {
		t.Fatal("cooldown must block")
	}
	now = now.Add(59 * time.Second)
	if !f.blocked("2") {
		t.Fatal("still inside the cooldown")
	}
	now = now.Add(2 * time.Second)
	if f.blocked("2") {
		t.Fatal("the cooldown must expire — a transient failure is not forever")
	}

	f.blacklist("4")
	now = now.Add(24 * time.Hour)
	if !f.blocked("4") {
		t.Fatal("a blacklist must not expire")
	}
	f.clear("4")
	if !f.blocked("4") {
		t.Fatal("clear drops only a cooldown, not a blacklist")
	}
}

func TestDirectHealthRequiresReplyAndExpiresPerEndpoint(t *testing.T) {
	now := time.Unix(1000, 0)
	f := newFailTracker(func() time.Time { return now }, time.Minute)
	const endpoint = "149.154.167.51:443"
	if f.healthy(endpoint) {
		t.Fatal("unknown endpoint is not healthy")
	}
	f.replied(endpoint)
	if !f.healthy(endpoint) {
		t.Fatal("reply should establish health")
	}
	if f.healthy("149.154.167.51:80") {
		t.Fatal("another port is not proven")
	}
	f.cooldown("149.154.167.51:80")
	if !f.healthy(endpoint) {
		t.Fatal("port 80 failure must not poison port 443")
	}
	now = now.Add(time.Minute)
	if f.healthy(endpoint) {
		t.Fatal("stale evidence must expire")
	}
	f.replied(endpoint)
	f.cooldown(endpoint)
	if f.healthy(endpoint) {
		t.Fatal("a failure must invalidate positive evidence")
	}
}
