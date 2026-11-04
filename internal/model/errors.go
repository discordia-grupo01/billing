package model

import "errors"

// ErrNotFound means the row doesn't exist in our database. Errors from the
// payment provider use service.ErrNotFound instead.
var ErrNotFound = errors.New("not found")

// ErrLiveSubscriptionExists means the user already has another subscription
// that MP keeps charging (authorized or paused).
var ErrLiveSubscriptionExists = errors.New("user already has a live subscription")
