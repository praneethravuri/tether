//go:build !linux && !darwin

package proc

import "errors"

// StartTime has no implementation on this platform.
func StartTime(int) (int64, error) {
	return 0, errors.New("tether: process start time is not available on this platform")
}
