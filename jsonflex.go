package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// The asset API is loose about scalar types: user_id comes back as a string on
// one endpoint and as a bare number on another, and amounts are quoted strings
// ("-68"). Decoding straight into int64/string fails the whole payload, so the
// fields that vary use these types instead.

// flexString accepts a JSON string, number, bool or null.
type flexString string

func (f *flexString) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*f = ""
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.Trim(string(raw), `"`))
	return nil
}

func (f flexString) String() string { return string(f) }

// flexInt accepts a JSON number, numeric string, bool or null.
type flexInt int64

func (f *flexInt) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*f = 0
		return nil
	}
	text := strings.Trim(string(raw), `"`)
	if text == "" || text == "true" || text == "false" {
		*f = 0
		return nil
	}
	if v, err := strconv.ParseInt(text, 10, 64); err == nil {
		*f = flexInt(v)
		return nil
	}
	// Tolerate "1.0" and other float-shaped integers.
	if v, err := strconv.ParseFloat(text, 64); err == nil {
		*f = flexInt(int64(v))
		return nil
	}
	*f = 0
	return nil
}

func (f flexInt) Int64() int64 { return int64(f) }

// flexFloat accepts a JSON number, numeric string or null.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*f = 0
		return nil
	}
	text := strings.Trim(string(raw), `"`)
	if text == "" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexFloat(v)
	return nil
}

func (f flexFloat) Float64() float64 { return float64(f) }

// flexID keeps a large snowflake id intact: it may arrive as a JSON number
// (which would lose precision in float64) or as a string.
type flexID string

func (f *flexID) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		*f = ""
		return nil
	}
	*f = flexID(strings.Trim(string(raw), `"`))
	return nil
}

func (f flexID) String() string { return string(f) }
