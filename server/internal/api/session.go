package api

import "net/http"

// SessionResolver implements the hub/up SessionUserResolver contract using the
// stateless signed-cookie session defined in security.go (cookie name
// pushfree_session, value "<uid>:<exp>.<base64url hmac>"). It is the real
// wiring seam used by server.MountRealtime: the same *SessionResolver instance
// is passed to hub.New and up.New, so device registration under /1/devices and
// /up/{sub} share one cookie-session implementation.
//
// It deliberately does NOT import the hub package: Go structural typing means a
// type with the matching ResolveUserID method satisfies hub.SessionUserResolver
// and up.SessionUserResolver without a compile-time dependency.
type SessionResolver struct {
	secret []byte
}

// NewSessionResolver builds a resolver that validates the session cookie signed
// by authSecret (the same secret Accounts uses to sign logins).
func NewSessionResolver(authSecret []byte) *SessionResolver {
	return &SessionResolver{secret: authSecret}
}

// ResolveUserID returns the authenticated user id from the session cookie, or
// ok=false if the cookie is absent, tampered, or expired. It reuses parseSession
// so the validity rules (HMAC equality, expiry) are exactly those of the
// account login flow -- there is no duplicated auth logic here.
func (s *SessionResolver) ResolveUserID(r *http.Request) (int64, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return 0, false
	}
	return parseSession(s.secret, c.Value)
}
