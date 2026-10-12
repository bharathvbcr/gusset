package ffi

import (
	"errors"
	"testing"
)

// statusToError is only reached because an export returned a non-OK code. A
// status that is nil or says FFI_OK contradicts that, and returning nil made
// the caller hand back a zero handle, buffer or ticket with a nil error.
func TestStatusToError_DisagreementIsAnError(t *testing.T) {
	var ok ffiStatus // zero value: code FFI_OK, no message
	err := statusToError(&ok)
	if err == nil {
		t.Fatal("an error return with an FFI_OK status must be an error, got nil")
	}
	if !errors.Is(err, ErrGeneric) {
		t.Fatalf("want ErrGeneric, got %v", err)
	}

	if err := statusToError(nil); err == nil {
		t.Fatal("an error return with no status must be an error, got nil")
	}
}
