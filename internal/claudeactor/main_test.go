package claudeactor

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// With POMAR_CC_MCP_SOCKET set, the test binary is Pomar's MCP server for a
// live Claude Code (the live tests start it from --mcp-config), listing the
// controller tool for POMAR_CC_MCP_CONTROLLER's capabilities, if set.
func TestMain(m *testing.M) {
	if s := os.Getenv("POMAR_CC_MCP_SOCKET"); s != "" {
		var capabilities []string
		if c := os.Getenv("POMAR_CC_MCP_CONTROLLER"); c != "" {
			capabilities = strings.Split(c, ",")
		}
		err := ServeMCPController(os.Stdin, os.Stdout, capabilities, func(tool string, args json.RawMessage) (bridgeReply, error) {
			return Forward(s, tool, args)
		})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
