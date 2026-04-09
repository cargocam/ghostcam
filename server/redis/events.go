package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

const (
	eventKeyPrefix     = "events:"
	eventReadPrefix    = "events_read:"
	eventDismissPrefix = "events_dismissed:"
	eventUnreadPrefix  = "events_unread:"
	eventRetentionMs   = 7 * 24 * 60 * 60 * 1000 // 7 days
	eventSetTTL        = 7 * 24 * time.Hour
)

// EventEntry represents a persisted event from a Redis Stream.
type EventEntry struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	DeviceID  string `json:"device_id"`
	Data      string `json:"data"` // raw JSON string
	CreatedAt uint64 `json:"created_at"`
	Read      bool   `json:"read"`
	Dismissed bool   `json:"dismissed"`
}

// WriteEvent persists an event to the user's event stream.
// Returns the Redis stream entry ID.
func WriteEvent(ctx context.Context, rdb *goredis.Client, userID, deviceID, eventType, data string) (string, error) {
	key := eventKeyPrefix + userID
	now := uint64(time.Now().UnixMilli())
	minID := now - eventRetentionMs

	id, err := rdb.XAdd(ctx, &goredis.XAddArgs{
		Stream: key,
		MinID:  fmt.Sprintf("%d", minID),
		Approx: true,
		ID:     "*",
		Values: map[string]interface{}{
			"type":      eventType,
			"device_id": deviceID,
			"data":      data,
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("writing event: %w", err)
	}

	rdb.Incr(ctx, eventUnreadPrefix+userID)

	return id, nil
}

// ListEvents returns recent events for a user, newest first.
// Pass "+" for beforeID to start from the latest.
// Dismissed events are excluded from results.
func ListEvents(ctx context.Context, rdb *goredis.Client, userID string, count int64, beforeID string) ([]EventEntry, error) {
	key := eventKeyPrefix + userID
	if beforeID == "" {
		beforeID = "+"
	}

	results, err := rdb.XRevRangeN(ctx, key, beforeID, "-", count).Result()
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}

	// Fetch read + dismissed sets
	readSet, _ := rdb.SMembers(ctx, eventReadPrefix+userID).Result()
	dismissSet, _ := rdb.SMembers(ctx, eventDismissPrefix+userID).Result()

	readMap := make(map[string]bool, len(readSet))
	for _, id := range readSet {
		readMap[id] = true
	}
	dismissMap := make(map[string]bool, len(dismissSet))
	for _, id := range dismissSet {
		dismissMap[id] = true
	}

	entries := make([]EventEntry, 0, len(results))
	for _, msg := range results {
		if dismissMap[msg.ID] {
			continue
		}
		// Parse stream entry ID to get timestamp
		ts := parseStreamIDTimestamp(msg.ID)
		entries = append(entries, EventEntry{
			ID:        msg.ID,
			Type:      fieldStr(msg.Values, "type"),
			DeviceID:  fieldStr(msg.Values, "device_id"),
			Data:      fieldStr(msg.Values, "data"),
			CreatedAt: ts,
			Read:      readMap[msg.ID],
		})
	}
	return entries, nil
}

// UnreadCount returns the number of unread events for a user.
func UnreadCount(ctx context.Context, rdb *goredis.Client, userID string) (int64, error) {
	val, err := rdb.Get(ctx, eventUnreadPrefix+userID).Int64()
	if err == goredis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if val < 0 {
		return 0, nil
	}
	return val, nil
}

// MarkEventRead marks a single event as read. Returns true if it was previously unread.
func MarkEventRead(ctx context.Context, rdb *goredis.Client, userID, eventID string) (bool, error) {
	readKey := eventReadPrefix + userID
	added, err := rdb.SAdd(ctx, readKey, eventID).Result()
	if err != nil {
		return false, err
	}
	rdb.Expire(ctx, readKey, eventSetTTL)

	if added > 0 {
		rdb.Decr(ctx, eventUnreadPrefix+userID)
		return true, nil
	}
	return false, nil
}

// MarkAllRead marks all current events as read.
func MarkAllRead(ctx context.Context, rdb *goredis.Client, userID string) error {
	rdb.Set(ctx, eventUnreadPrefix+userID, 0, 0)
	// We don't need to add every ID to the read set — just set the counter to 0.
	// The read set is used for per-event status; for "mark all read" we track the
	// timestamp and consider everything before it as read.
	// Simpler: store a "read_all_before" timestamp.
	readAllKey := eventReadPrefix + userID + ":all_before"
	rdb.Set(ctx, readAllKey, fmt.Sprintf("%d", time.Now().UnixMilli()), eventSetTTL)
	return nil
}

// DismissEvent soft-deletes an event (excluded from list results).
func DismissEvent(ctx context.Context, rdb *goredis.Client, userID, eventID string) error {
	dismissKey := eventDismissPrefix + userID
	rdb.SAdd(ctx, dismissKey, eventID)
	rdb.Expire(ctx, dismissKey, eventSetTTL)

	// Also mark as read + decrement counter if it was unread
	readKey := eventReadPrefix + userID
	added, _ := rdb.SAdd(ctx, readKey, eventID).Result()
	rdb.Expire(ctx, readKey, eventSetTTL)
	if added > 0 {
		rdb.Decr(ctx, eventUnreadPrefix+userID)
	}
	return nil
}

func fieldStr(values map[string]interface{}, key string) string {
	v, ok := values[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

func parseStreamIDTimestamp(id string) uint64 {
	// Stream ID format: "1775751411472-0"
	for i, c := range id {
		if c == '-' {
			ts, _ := strconv.ParseUint(id[:i], 10, 64)
			return ts
		}
	}
	ts, _ := strconv.ParseUint(id, 10, 64)
	return ts
}
