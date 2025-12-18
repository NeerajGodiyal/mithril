package snapshot

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Overclock-Validator/mithril/pkg/mlog"
)

type bufmonreader struct {
	name      string
	b         io.Reader
	c         io.Closer
	bytesRead int64
	totalSize int64
	startTime time.Time
}

const gib = 1 << 30

func NewBufMonReader(name string, r io.ReadCloser, totalSize int64) *bufmonreader {
	return &bufmonreader{
		name:      name,
		b:         r,
		totalSize: totalSize,
		startTime: time.Now(),
	}
}

func NewBufMonReaderFromFile(file *os.File) (*bufmonreader, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	return &bufmonreader{
		name:      file.Name(),
		b:         bufio.NewReader(file),
		c:         file,
		totalSize: info.Size(),
		startTime: time.Now(),
	}, nil
}

func NewBufMonReaderHTTP(url string) (*bufmonreader, error) {
	resp, err := http.Head(url)
	if err != nil {
		return nil, fmt.Errorf("HEAD %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HEAD %s: had not-ok status: %s", url, resp.Status)
	}
	totalSize := resp.ContentLength
	resp.Body.Close()
	resp, err = http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: had not-ok status: %s", url, resp.Status)
	}

	return &bufmonreader{
		name:      url,
		b:         resp.Body,
		c:         resp.Body,
		totalSize: totalSize,
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
			x.name,
			remaining.Round(time.Second))
	}
	return n, err
}

func (x *bufmonreader) Close() error {
	return x.c.Close()
}
