//go:build !lean || openai

package main

// The model provider openai, which also speaks to any OpenAI-compatible
// server.
import _ "github.com/bdubs00/featherweight-kernel/model/openai"
