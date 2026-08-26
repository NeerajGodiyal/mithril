package sealevel

// DefaultLogMessagesBytesLimit matches Agave's per-transaction recorder cap.
const DefaultLogMessagesBytesLimit = uint64(10 * 1000)

type Logger interface {
	Log(s string)
}

type LogRecorder struct {
	Logs         []string
	BytesLimit   uint64
	BytesWritten uint64
	Truncated    bool
}

func (r *LogRecorder) Log(s string) {
	if r.BytesLimit != 0 && uint64(len(s)) > r.BytesLimit-r.BytesWritten {
		if !r.Truncated {
			r.Truncated = true
			r.Logs = append(r.Logs, "Log truncated")
		}
		return
	}
	r.BytesWritten += uint64(len(s))
	r.Logs = append(r.Logs, s)
}
