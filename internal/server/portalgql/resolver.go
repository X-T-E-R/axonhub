package portalgql

import "github.com/looplj/axonhub/internal/server/biz"

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Service *biz.PortalService
	Users   *biz.UserService
}
