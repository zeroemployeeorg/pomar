package claudeactor

import (
	"encoding/json"
	"os"
	"testing"
)

// With POMAR_CC_MCP_SOCKET set, the test binary is Pomar's MCP server for a
// live Claude Code (the live tests start it from --mcp-config).
func TestMain(m *testing.M) {
	if s := os.Getenv("POMAR_CC_MCP_SOCKET"); s != "" {
		err := ServeMCP(os.Stdin, os.Stdout, func(tool string, args json.RawMessage) (bridgeReply, error) {
			return Forward(s, tool, args)
		})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
