package titleexport

import (
	"bytes"
	"time"
)

func newReader(raw []byte) *bytes.Reader { return bytes.NewReader(raw) }

func parseTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
