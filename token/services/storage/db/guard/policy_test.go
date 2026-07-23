/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package guard

import "testing"

// stubConfig is a minimal driver.Config for exercising LoadPolicy.
type stubConfig struct {
	values map[string]int
}

func (c *stubConfig) IsSet(key string) bool {
	_, ok := c.values[key]

	return ok
}

func (c *stubConfig) UnmarshalKey(key string, rawVal any) error {
	if v, ok := c.values[key]; ok {
		*rawVal.(*int) = v
	}

	return nil
}

func TestLoadPolicyDefaults(t *testing.T) {
	p, err := LoadPolicy(&stubConfig{values: map[string]int{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.MaxPayloadSize != DefaultMaxPayloadSize {
		t.Fatalf("expected default payload size %d, got %d", DefaultMaxPayloadSize, p.MaxPayloadSize)
	}
	if p.MaxPageSize != DefaultMaxPageSize {
		t.Fatalf("expected default page size %d, got %d", DefaultMaxPageSize, p.MaxPageSize)
	}
}

func TestLoadPolicyNilConfig(t *testing.T) {
	p, err := LoadPolicy(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != DefaultPolicy() {
		t.Fatalf("expected default policy for nil config, got %+v", p)
	}
}

func TestLoadPolicyOverrideAndDisable(t *testing.T) {
	// An explicit 0 disables the payload check; page size is overridden.
	p, err := LoadPolicy(&stubConfig{values: map[string]int{
		ConfigKeyMaxPayloadSize: 0,
		ConfigKeyMaxPageSize:    250,
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.MaxPayloadSize != 0 {
		t.Fatalf("expected payload size 0 (disabled), got %d", p.MaxPayloadSize)
	}
	if p.MaxPageSize != 250 {
		t.Fatalf("expected page size 250, got %d", p.MaxPageSize)
	}
}
