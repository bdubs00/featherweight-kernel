//go:build !lean || spiffe

package main

// The authentication mode spiffe and the TLS provider spiffe.
import _ "github.com/bdubs00/featherweight-kernel/authn/spiffe"
