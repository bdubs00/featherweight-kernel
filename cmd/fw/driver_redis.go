//go:build !lean || redis

package main

// The bus driver redis (bus.driver: redis).
import _ "github.com/bdubs00/featherweight-kernel/bus/redis"
