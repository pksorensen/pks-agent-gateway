package main

import "context"

const (
	RoleGatewayAdmin = "GatewayAdmin"
	RoleProjectAdmin = "ProjectAdmin"
	RoleProjectUser  = "ProjectUser"
)

// UserClaims holds identity and role information extracted from a verified token.
type UserClaims struct {
	Sub   string
	Email string
	Roles []string
}

// HasRole reports whether the user has the given role.
func (u *UserClaims) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsAdmin reports whether the user holds the GatewayAdmin role.
func (u *UserClaims) IsAdmin() bool {
	return u.HasRole(RoleGatewayAdmin)
}

// context key type — unexported to prevent collisions.
type contextKey string

const claimsKey contextKey = "claims"

// claimsFromCtx retrieves UserClaims from ctx, or nil if absent.
func claimsFromCtx(ctx context.Context) *UserClaims {
	v, _ := ctx.Value(claimsKey).(*UserClaims)
	return v
}

// withClaims returns a new context carrying the given UserClaims.
func withClaims(ctx context.Context, c *UserClaims) context.Context {
	return context.WithValue(ctx, claimsKey, c)
}
