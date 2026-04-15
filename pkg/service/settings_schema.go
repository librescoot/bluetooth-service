package service

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// settingSchema mirrors the subset of settings-service's schema that we need
// for validation. Kept as a local copy to avoid inverting the dependency
// direction (bluetooth-service should not import settings-service internals).
type settingSchema struct {
	Type     string               `json:"type"`
	Default  any                  `json:"default,omitempty"`
	Values   []settingSchemaValue `json:"values,omitempty"`
	Min      *float64             `json:"min,omitempty"`
	Max      *float64             `json:"max,omitempty"`
	ReadOnly bool                 `json:"read-only,omitempty"`
	Pattern  string               `json:"pattern,omitempty"`
}

type settingSchemaValue struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

const settingsSchemaKey = "settings:schema"

// loadSettingsSchema fetches the schema JSON settings-service publishes at
// startup and parses it. Callers should cache the result; the schema only
// changes when settings-service restarts.
func (s *Service) loadSettingsSchema() (map[string]settingSchema, error) {
	raw, err := s.ipc.Get(settingsSchemaKey)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", settingsSchemaKey, err)
	}
	if raw == "" {
		return nil, fmt.Errorf("%s is empty (settings-service not ready?)", settingsSchemaKey)
	}
	var parsed map[string]settingSchema
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", settingsSchemaKey, err)
	}
	return parsed, nil
}

// getSettingsSchema returns the cached schema, loading it on first use.
// The cache is invalidated on load failure so a later call retries.
func (s *Service) getSettingsSchema() (map[string]settingSchema, error) {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if s.schemaCache != nil {
		return s.schemaCache, nil
	}
	schema, err := s.loadSettingsSchema()
	if err != nil {
		return nil, err
	}
	s.schemaCache = schema
	return schema, nil
}

// validateSettingValue checks a proposed value against the schema entry.
// Returns a human-readable error string suitable for a BT response, or ""
// if the value is acceptable.
func validateSettingValue(spec settingSchema, value string) string {
	switch spec.Type {
	case "bool":
		if value != "true" && value != "false" {
			return "invalid bool"
		}
	case "int":
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return "invalid int"
		}
		if spec.Min != nil && float64(n) < *spec.Min {
			return "out of range"
		}
		if spec.Max != nil && float64(n) > *spec.Max {
			return "out of range"
		}
	case "float":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return "invalid float"
		}
		if spec.Min != nil && n < *spec.Min {
			return "out of range"
		}
		if spec.Max != nil && n > *spec.Max {
			return "out of range"
		}
	case "enum":
		for _, v := range spec.Values {
			if v.Value == value {
				return ""
			}
		}
		return "invalid enum"
	case "url":
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "invalid url"
		}
	case "duration":
		if _, err := time.ParseDuration(value); err != nil {
			return "invalid duration"
		}
	case "string":
		// pass through, no validation beyond non-empty key/value split
	default:
		// Unknown type (future schema extension): accept the raw string to
		// avoid blocking rollout of new types before this service updates.
	}
	return ""
}

// splitSetPayload parses "<key>:<value>" from the body of a set: command.
// Value may itself contain colons (URLs, pipe-delimited strings), so we
// split on the FIRST colon only.
func splitSetPayload(body string) (key, value string, ok bool) {
	idx := strings.Index(body, ":")
	if idx <= 0 || idx == len(body)-1 {
		return "", "", false
	}
	return body[:idx], body[idx+1:], true
}
