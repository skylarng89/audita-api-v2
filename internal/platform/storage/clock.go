package storage

import "time"

// timeNowUTC is a seam for tests.
var timeNowUTC = func() time.Time { return time.Now().UTC() }
