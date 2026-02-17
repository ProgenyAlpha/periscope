package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/ProgenyAlpha/periscope/internal/store"
)

// ensureVAPIDKeys generates a VAPID key pair if missing, stores in KV, and returns them.
func ensureVAPIDKeys(db *sql.DB) (pub, priv string, err error) {
	pubRaw := store.KVGet(db, "config:vapid-public")
	privRaw := store.KVGet(db, "config:vapid-private")
	if pubRaw != nil && privRaw != nil {
		var p, k string
		if json.Unmarshal(pubRaw, &p) == nil && json.Unmarshal(privRaw, &k) == nil && p != "" && k != "" {
			return p, k, nil
		}
	}

	priv, pub, err = webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", fmt.Errorf("generate VAPID keys: %w", err)
	}

	pubJSON, _ := json.Marshal(pub)
	privJSON, _ := json.Marshal(priv)
	store.KVSet(db, "config:vapid-public", string(pubJSON))
	store.KVSet(db, "config:vapid-private", string(privJSON))
	slog.Info("generated new VAPID key pair")
	return pub, priv, nil
}

// sendPushNotification sends a push notification to all subscribers.
func sendPushNotification(db *sql.DB, title, body string) error {
	pub, priv, err := ensureVAPIDKeys(db)
	if err != nil {
		return err
	}

	subs, err := store.PushGetAll(db)
	if err != nil {
		return err
	}
	if len(subs) == 0 {
		slog.Debug("no push subscribers, skipping")
		return nil
	}

	payload, _ := json.Marshal(map[string]string{"title": title, "body": body})

	sent, failed := 0, 0
	for _, sub := range subs {
		s := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				Auth:   sub.Auth,
				P256dh: sub.P256dh,
			},
		}
		resp, err := webpush.SendNotification(payload, s, &webpush.Options{
			Subscriber:      "mailto:periscope@localhost",
			VAPIDPublicKey:  pub,
			VAPIDPrivateKey: priv,
			TTL:             60,
		})
		if err != nil {
			slog.Warn("push send failed", "endpoint", sub.Endpoint[:40], "err", err)
			failed++
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusGone {
			store.PushUnsubscribe(db, sub.Endpoint)
			slog.Info("pruned stale push endpoint", "endpoint", sub.Endpoint[:40])
			failed++
		} else {
			sent++
		}
	}
	slog.Info("push notification sent", "ok", sent, "failed", failed)
	return nil
}

// checkAndNotify evaluates rate limit thresholds and sends push alerts with cooldown.
func checkAndNotify(app *App, usage map[string]any) {
	pct5hr, _ := usage["pct5hr"].(float64)

	if pct5hr >= 90 {
		if !pushCooldownExpired(app.DB, "push:cooldown:5hr-critical", 30*time.Minute) {
			return
		}
		sendPushNotification(app.DB, "Periscope", fmt.Sprintf("5hr limit at %.0f%%", pct5hr))
		setPushCooldown(app.DB, "push:cooldown:5hr-critical")
	} else if pct5hr >= 80 {
		if !pushCooldownExpired(app.DB, "push:cooldown:5hr-warning", 30*time.Minute) {
			return
		}
		sendPushNotification(app.DB, "Periscope", fmt.Sprintf("Approaching 5hr limit: %.0f%%", pct5hr))
		setPushCooldown(app.DB, "push:cooldown:5hr-warning")
	} else if pct5hr > 0 && pct5hr < 20 {
		// Check if this is a reset (was previously high)
		lastRaw := store.KVGet(app.DB, "push:last-pct5hr")
		if lastRaw != nil {
			var lastPct float64
			if json.Unmarshal(lastRaw, &lastPct) == nil && lastPct >= 80 {
				if pushCooldownExpired(app.DB, "push:cooldown:5hr-reset", 30*time.Minute) {
					sendPushNotification(app.DB, "Periscope", "5hr window reset")
					setPushCooldown(app.DB, "push:cooldown:5hr-reset")
				}
			}
		}
	}

	// Track last pct5hr for reset detection
	if pct5hr > 0 {
		pctJSON, _ := json.Marshal(pct5hr)
		store.KVSet(app.DB, "push:last-pct5hr", string(pctJSON))
	}
}

func pushCooldownExpired(db *sql.DB, key string, duration time.Duration) bool {
	raw := store.KVGet(db, key)
	if raw == nil {
		return true
	}
	var ts string
	if json.Unmarshal(raw, &ts) != nil {
		return true
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return true
	}
	return time.Since(t) >= duration
}

func setPushCooldown(db *sql.DB, key string) {
	ts, _ := json.Marshal(time.Now().Format(time.RFC3339))
	store.KVSet(db, key, string(ts))
}
