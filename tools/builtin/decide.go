package builtin

import "github.com/bdubs00/featherweight-kernel/fw"

func init() { fw.RegisterTool("decide", newDecide) }

// newDecide offers the tool decide of the kernel (fw.DecideTool) to the
// model only when the configuration has a decider. Without one, the tool
// is off, and the kernel leaves it out.
func newDecide(cfg *fw.Config) (fw.Tool, error) {
	if cfg == nil || len(cfg.Deciders) == 0 {
		return nil, nil
	}
	return fw.DecideTool(), nil
}
