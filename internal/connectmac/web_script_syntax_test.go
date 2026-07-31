package connectmac

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestWebInlineScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required to validate web JavaScript syntax")
	}

	script := filepath.Join("..", "..", "scripts", "check-web-js.mjs")
	output, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("web JavaScript syntax check failed: %v\n%s", err, output)
	}
}
