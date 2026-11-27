package landmarks

import "time"

// Landmark is a named point of interest (ghat, temple, station, etc.) used
// to compute real walking/driving distance from properties in the same
// city - platform-wide reference data managed by admins, not vendors.
type Landmark struct {
	LandmarkID int64     `json:"landmark_id"`
	City       string    `json:"city"`
	Name       string    `json:"name"`
	Category   string    `json:"category,omitempty"`
	Latitude   float64   `json:"latitude"`
	Longitude  float64   `json:"longitude"`
	IsActive   bool      `json:"is_active"`
	CreatedAt  time.Time `json:"created_at"`
}
