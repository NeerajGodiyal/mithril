package snapshot

import (
	"bufio"
	"os"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

type bufmonreader struct {
	f         *os.File
	b         *bufio.Reader
	bytesRead int64
	totalSize int64
	startTime time.Time
}

const gib = 1 << 30

func NewBufMonReader(file *os.File) (*bufmonreader, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	return &bufmonreader{
		f:         file,
		b:         bufio.NewReader(file),
		totalSize: info.Size(),
		startTime: time.Now(),
	}, nil
}

func (x *bufmonreader) Read(p []byte) (int, error) {
	n, err := x.b.Read(p)
	before := x.bytesRead
	x.bytesRead += int64(n)

	if (x.bytesRead / gib) > (before / gib) {
		percentComplete := float64(x.bytesRead) / float64(x.totalSize) * 100

		elapsed := time.Since(x.startTime)
		var remaining time.Duration
		if x.bytesRead > 0 {
			bytesPerSec := float64(x.bytesRead) / elapsed.Seconds()
			remainingSecs := float64(x.totalSize-x.bytesRead) / bytesPerSec
			remaining = time.Duration(remainingSecs * float64(time.Second))
		}

		mlog.Log.Infof("Processed %.2f of %.2f GiB (%.1f%%) of %s. Est. remaining: %s",
			float64(x.bytesRead)/float64(gib),
			float64(x.totalSize)/float64(gib),
			percentComplete,
			x.f.Name(),
			remaining.Round(time.Second))
	}
	return n, err
}
