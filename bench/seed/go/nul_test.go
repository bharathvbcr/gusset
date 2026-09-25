package ffibench

import "testing"

func TestGuardedNUL(t *testing.T) {
	_, err := Guarded(2)
	t.Log("survived:", err)
}
