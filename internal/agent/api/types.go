//go:build linux

package api

import (
	"time"
)

// LeaseRequest represents a request to allocate a new lease
type LeaseRequest struct {
	Interface  string `json:"interface"`
	WanParent  string `json:"wanParent"`
	MACAddress string `json:"macAddress"`
}

// LeaseResponse represents the response for a lease allocation
type LeaseResponse struct {
	IPAddress string    `json:"ipAddress"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// LeaseStatus represents the status of a lease
type LeaseStatus struct {
	IPAddress       string    `json:"ipAddress"`
	ExpiresAt       time.Time `json:"expiresAt"`
	RenewalCount    int       `json:"renewalCount"`
	Status          string    `json:"status"`          // "active" or "stale"
	InterfaceExists bool      `json:"interfaceExists"` // true if kernel interface exists
}

// LeaseListResponse represents the response for listing leases
type LeaseListResponse struct {
	Leases []LeaseStatus `json:"leases"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}
