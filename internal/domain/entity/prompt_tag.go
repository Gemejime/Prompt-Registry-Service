package entity

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var tagRegex = regexp.MustCompile(`^@[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

// Standard semantic pointer tags.
const (
	TagProduction = "@production"
	TagStaging    = "@staging"
	TagCanary     = "@canary"
)

// PromptTag represents a mutable semantic tag pointer (@production, @staging, @canary)
// bound to a specific immutable PromptVersion.
type PromptTag struct {
	ID            string    `json:"id"`
	TenantID      string    `json:"tenant_id"`
	TemplateID    string    `json:"template_id"`
	Tag           string    `json:"tag"`
	TargetVersion int       `json:"target_version"`
	UpdatedBy     string    `json:"updated_by"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ValidateTagName verifies that a semantic tag starts with '@' and adheres to length/character constraints.
func ValidateTagName(tag string) error {
	tag = strings.TrimSpace(tag)
	if !tagRegex.MatchString(tag) {
		return fmt.Errorf("%w: invalid semantic tag %q (must start with '@' followed by alphanumeric characters, e.g. @production)", ErrInvalidInput, tag)
	}
	return nil
}

// ValidateTagPromotion enforces optimistic locking and version bounds for PromoteTag.
// Returns isRollback = true when an existing pointer is moved to an earlier version (targetVersion < currentVersion).
func ValidateTagPromotion(currentVersion int, expectedVersion int, targetVersion int) (bool, error) {
	if targetVersion < 1 {
		return false, fmt.Errorf("%w: target_version must be >= 1", ErrInvalidInput)
	}
	if expectedVersion < 0 {
		return false, fmt.Errorf("%w: expected_version must be >= 0", ErrInvalidInput)
	}
	if currentVersion != expectedVersion {
		return false, fmt.Errorf("%w: expected pointer version %d, but current version is %d", ErrVersionConflict, expectedVersion, currentVersion)
	}
	isRollback := currentVersion > 0 && targetVersion < currentVersion
	return isRollback, nil
}
