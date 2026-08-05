package domain

import "errors"

// ErrNotFound is what repositories return when a lookup finds nothing. The
// application layer translates it into the protocol error each method expects,
// since the same miss means different things per method.
var ErrNotFound = errors.New("not found")

// ErrDuplicate reports that a transaction with the same Payme identifier
// already exists. It means a concurrent delivery of the same request won the
// race, so the caller replays the stored response instead of failing.
var ErrDuplicate = errors.New("transaction already exists")
