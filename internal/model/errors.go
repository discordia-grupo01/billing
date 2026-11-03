package model

import "errors"

// ErrNotFound means the row doesn't exist in our database. Errors from the
// payment provider use service.ErrNotFound instead.
var ErrNotFound = errors.New("not found")
