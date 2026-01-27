package main

import (
	"strings"
	"testing"
)

// TestNewUniqueIDUniqueness verifies that each call to NewUniqueID() returns a unique ID
func TestNewUniqueIDUniqueness(t *testing.T) {
	count := 1000
	ids := make(map[UniqueID]bool)

	for i := 0; i < count; i++ {
		id := NewUniqueID()
		if ids[id] {
			t.Errorf("Duplicate ID generated: %s (iteration %d)", id, i)
		}
		ids[id] = true
	}

	if len(ids) != count {
		t.Errorf("Expected %d unique IDs, got %d", count, len(ids))
	}
}

// TestNewUniqueIDChronologicalOrder verifies that subsequent IDs maintain chronological order
func TestNewUniqueIDChronologicalOrder(t *testing.T) {
	count := 100
	ids := make([]UniqueID, count)

	for i := 0; i < count; i++ {
		ids[i] = NewUniqueID()
		// Small delay to ensure timestamp difference
		time.Sleep(1 * time.Millisecond)
	}

	for i := 1; i < count; i++ {
		if strings.Compare(string(ids[i-1]), string(ids[i])) >= 0 {
			t.Errorf("Chronological order violated at index %d: %s >= %s",
				i, ids[i-1], ids[i])
		}
	}
}

// TestNewUniqueIDFormat verifies the format and length of generated IDs
func TestNewUniqueIDFormat(t *testing.T) {
	id := NewUniqueID()

	// Base32Hex encoding of 16 bytes should produce a string
	// 16 bytes * 8 bits = 128 bits
	// Base32 uses 5 bits per character, so 128/5 = 25.6, rounded up to 26 characters
	expectedLen := 26
	if len(id) != expectedLen {
		t.Errorf("Expected ID length %d, got %d: %s", expectedLen, len(id), id)
	}

	// Verify that the ID only contains valid Base32Hex characters (0-9, A-V, without padding)
	validChars := "0123456789ABCDEFGHIJKLMNOPQRSTUV"
	for _, char := range id {
		if !strings.ContainsRune(validChars, char) {
			t.Errorf("Invalid character in ID: %c (in %s)", char, id)
		}
	}
}

// TestNewUniqueIDNeverEmpty verifies that generated IDs are never empty
func TestNewUniqueIDNeverEmpty(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := NewUniqueID()
		if len(id) == 0 {
			t.Error("Generated empty UniqueID")
		}
	}
}

// TestNewUniqueIDStringConversion verifies type conversion works correctly
func TestNewUniqueIDStringConversion(t *testing.T) {
	id := NewUniqueID()
	idStr := string(id)

	if len(idStr) == 0 {
		t.Error("String conversion resulted in empty string")
	}

	// Verify that converting back to UniqueID works
	idAgain := UniqueID(idStr)
	if id != idAgain {
		t.Errorf("Round-trip conversion failed: %s != %s", id, idAgain)
	}
}
