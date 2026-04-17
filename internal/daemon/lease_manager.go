package daemon

// This file re-exports types from internal/daemon/lease so that existing code
// referencing daemon.LeaseManager, daemon.NewLeaseManager, etc. continues to compile.

import (
	"github.com/msageha/maestro_v2/internal/daemon/lease"
)

// LeaseManager is an alias for lease.Manager.
type LeaseManager = lease.Manager

// LeaseManagerOption is an alias for lease.Option.
type LeaseManagerOption = lease.Option

// LeaseInfo is an alias for lease.Info.
type LeaseInfo = lease.Info

// NewLeaseManager is an alias for lease.New.
var NewLeaseManager = lease.New

// WithLeaseManagerClock is an alias for lease.WithClock.
var WithLeaseManagerClock = lease.WithClock
