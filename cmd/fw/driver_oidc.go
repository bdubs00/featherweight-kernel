//go:build !lean || oidc

package main

// The authentication mode oidc, which verifies the tokens of people. A
// worker that people reach through the front needs it, because the front
// forwards their credentials.
import _ "github.com/bdubs00/featherweight-kernel/authn/oidc"
