// Package mfa holds the values of multi-factor authentication that need no
// database: the policy of ASTOVIA_MFA_REQUIRED, the one-time codes of
// authenticator apps (TOTP, RFC 6238) and the recovery codes.
package mfa

// Policies of ASTOVIA_MFA_REQUIRED. An empty policy is PolicyOff.
const (
	PolicyOff    = "off"
	PolicyOwners = "owners"
	PolicyAll    = "all"
)

// Required reports whether a user must have a second factor under policy.
// owner tells whether the user's role is owner.
func Required(policy string, owner bool) bool {
	return policy == PolicyAll || (policy == PolicyOwners && owner)
}
