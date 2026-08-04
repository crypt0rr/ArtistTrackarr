package store

import (
	"errors"
	"strings"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

func destinationName(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("destination name is required")
	}
	if utf8.RuneCountInString(value) > 80 {
		return "", errors.New("destination name must be 80 characters or fewer")
	}
	return value, nil
}
