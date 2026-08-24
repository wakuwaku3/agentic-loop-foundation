package contracts

import (
	"fmt"
	"time"
)

// validateProviderStandingAuthorization enforces the one rule of the V2-050
// runaway-guard contract that the restricted schema keyword subset in
// internal/contracts/validator.go cannot express: a cross-field comparison
// between two sibling date-time fields. (validator.go implements only $ref,
// const, enum, type, required, properties, additionalProperties:false,
// minItems, items, minLength, maxLength, pattern, format, minimum, maximum,
// none of which can compare one field's value against another field's
// value.) It is invoked by Validate for every value whose kind is
// "provider-standing-authorization", so it also runs for the fixtures in
// TestSchemasAndFixtures.
//
//  1. approved_at must not be later than created_at: the record documents
//     an authorization that was already granted by the time the record was
//     written, so the record cannot postdate its own approval.
//
// approver's email-shaped format and providers' non-empty, enum-constrained
// membership are both fully expressible with schema keywords alone (pattern
// and minItems/enum respectively) and are therefore enforced by
// contracts/schemas/provider-standing-authorization.json, not here.
func validateProviderStandingAuthorization(record map[string]any) error {
	createdAt, err := time.Parse(time.RFC3339, stringValue(record["created_at"]))
	if err != nil {
		return fmt.Errorf("/created_at: %w", err)
	}
	approvedAt, err := time.Parse(time.RFC3339, stringValue(record["approved_at"]))
	if err != nil {
		return fmt.Errorf("/approved_at: %w", err)
	}
	if approvedAt.After(createdAt) {
		return fmt.Errorf("/approved_at: must not be later than created_at")
	}
	return nil
}
