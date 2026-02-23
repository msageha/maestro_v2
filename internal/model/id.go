package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

type IDType string

const (
	IDTypeCommand      IDType = "cmd"
	IDTypeTask         IDType = "task"
	IDTypePhase        IDType = "phase"
	IDTypeNotification IDType = "ntf"
	IDTypeResult       IDType = "res"
)

var validIDTypes = map[IDType]bool{
	IDTypeCommand:      true,
	IDTypeTask:         true,
	IDTypePhase:        true,
	IDTypeNotification: true,
	IDTypeResult:       true,
}

var idRegex = regexp.MustCompile(`^(cmd|task|phase|ntf|res)_[0-9]{10}_[0-9a-f]{8}$`)

func GenerateID(idType IDType) (string, error) {
	if !validIDTypes[idType] {
		return "", fmt.Errorf("invalid ID type: %s", idType)
	}

	timestamp := time.Now().Unix()
	randomBytes := make([]byte, 4)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	hexStr := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("%s_%010d_%s", idType, timestamp, hexStr), nil
}

func ValidateID(id string) bool {
	return idRegex.MatchString(id)
}

func ParseIDType(id string) (IDType, error) {
	if !ValidateID(id) {
		return "", fmt.Errorf("invalid ID format: %s", id)
	}
	match := idRegex.FindStringSubmatch(id)
	return IDType(match[1]), nil
}

func ParseIDTimestamp(id string) (time.Time, error) {
	if !ValidateID(id) {
		return time.Time{}, fmt.Errorf("invalid ID format: %s", id)
	}
	// Extract timestamp portion: after first '_', 10 digits
	tsStr := id[len(id)-19 : len(id)-9]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("failed to parse timestamp from ID %s: %w", id, err)
	}
	return time.Unix(ts, 0), nil
}
