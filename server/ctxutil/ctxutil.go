// Package ctxutil provides context key helpers shared between middleware and handlers.
package ctxutil

import "net/http"

type contextKey string

const (
	// KeyUserID is the context key for authenticated user ID.
	KeyUserID contextKey = "user_id"
	// KeyCameraDeviceID is the context key for authenticated camera device ID.
	KeyCameraDeviceID contextKey = "camera_device_id"
	// KeyCameraUserID is the context key for the camera's owner user ID.
	KeyCameraUserID contextKey = "camera_user_id"
)

// GetUserID extracts the authenticated user ID from the request context.
func GetUserID(r *http.Request) string {
	if v, ok := r.Context().Value(KeyUserID).(string); ok {
		return v
	}
	return ""
}

// GetCameraDeviceID extracts the authenticated camera device ID from the request context.
func GetCameraDeviceID(r *http.Request) string {
	if v, ok := r.Context().Value(KeyCameraDeviceID).(string); ok {
		return v
	}
	return ""
}
