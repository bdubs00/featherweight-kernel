//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package fw

import (
	"fmt"
	"os"
	"runtime"
)

func lockLedgerDirectory(string) (*os.File, error) {
	return nil, fmt.Errorf("fw: file ledger: exclusive writer ownership is unsupported on %s", runtime.GOOS)
}
