// Package fw is the public API of featherweight-kernel.
//
// It holds the interfaces that every component implements, the types that
// travel between them, the configuration, the option functions, the driver
// registry, and the kernel. The kernel imports no driver: drivers live in
// public packages and register themselves by name at init. A program selects
// drivers by importing them.
//
// The design is described in docs/ARCHITECTURE.md.
package fw
