package database

import "testing"

func TestIsSpaceQuotaError(t *testing.T) {
	if !isSpaceQuotaError(errString("you are over your space quota, using 514 MB of 512 MB")) {
		t.Fatal("expected space quota detection")
	}
	if isSpaceQuotaError(errString("connection refused")) {
		t.Fatal("unexpected quota detection")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
