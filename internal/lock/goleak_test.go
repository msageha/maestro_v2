// The lockorder build provides its own TestMain (lock_order_test.go) that
// enables panic-mode order checking; Go allows only one TestMain per
// package, so the goleak harness applies to the default build only.
// Without this constraint, `go test -tags lockorder ./internal/lock`
// failed at setup with a duplicate TestMain and the order-checker tests
// never ran.
//go:build !lockorder

package lock

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
