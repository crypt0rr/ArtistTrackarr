package web

import (
	"encoding/csv"
	"errors"
	"strings"
	"testing"
)

func TestBuildBufferedCSVDoesNotCommitPartialOutputOnBuildError(t *testing.T) {
	_, err := buildBufferedCSV(func(writer *csv.Writer) error {
		if err := writer.Write([]string{"header"}); err != nil {
			return err
		}
		return errors.New("query failed")
	})
	if err == nil || !strings.Contains(err.Error(), "query failed") {
		t.Fatalf("build error=%v, want query failure", err)
	}
}

func TestBuildBufferedCSVCapsOutput(t *testing.T) {
	_, err := buildBufferedCSV(func(writer *csv.Writer) error {
		return writer.Write([]string{strings.Repeat("x", maxBufferedCSVExportBytes)})
	})
	if !errors.Is(err, errCSVExportTooLarge) {
		t.Fatalf("cap error=%v, want %v", err, errCSVExportTooLarge)
	}
}
