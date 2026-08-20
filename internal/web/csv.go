package web

import (
	"bytes"
	"encoding/csv"
	"errors"
)

// CSV exports are assembled before response headers are committed. That way a
// query or serialization error produces a normal 500 response instead of a
// misleading 200 response containing a truncated, apparently valid file.
// Keep the buffer bounded so an unexpectedly large household cannot turn an
// export into an unbounded memory allocation.
const maxBufferedCSVExportBytes = 32 << 20

var errCSVExportTooLarge = errors.New("CSV export exceeds the 32 MiB safety limit")

type cappedCSVBuffer struct {
	bytes.Buffer
	max int
}

func (b *cappedCSVBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errCSVExportTooLarge
	}
	return b.Buffer.Write(p)
}

func buildBufferedCSV(build func(*csv.Writer) error) ([]byte, error) {
	buffer := &cappedCSVBuffer{max: maxBufferedCSVExportBytes}
	writer := csv.NewWriter(buffer)
	if err := build(writer); err != nil {
		return nil, err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return append([]byte(nil), buffer.Bytes()...), nil
}
