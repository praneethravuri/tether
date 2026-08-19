//go:build linux

package proc

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

var errBadProcStat = errors.New("tether: malformed /proc/<pid>/stat")

// StartTime reads pid's start time (clock ticks since boot) from
// /proc/<pid>/stat field 22.
func StartTime(pid int) (int64, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}

	s := string(raw)
	// comm (field 2) is parenthesized and unescaped, so scan for the last
	// ')' rather than splitting naively on spaces.
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 > len(s) {
		return 0, errBadProcStat
	}

	fields := strings.Fields(s[i+2:])
	const starttimeIndex = 22 - 3 // fields[0] is field 3 (state)
	if len(fields) <= starttimeIndex {
		return 0, errBadProcStat
	}

	return strconv.ParseInt(fields[starttimeIndex], 10, 64)
}
