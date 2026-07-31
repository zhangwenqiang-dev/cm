package connectmac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppWebLocalAgentSecureFallbackContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`secureURL: "https://127.0.0.1:18765"`,
		`legacyURL: "http://127.0.0.1:18765"`,
		`async function probeLocalAgent(url, timeoutMs = 1200)`,
		`if (!res.ok || !body?.ok)`,
		`[state.localAgent.secureURL, state.localAgent.legacyURL]`,
		`state.localAgent.errorReason = connectedURL ? "" : localAgentOfflineMessage;`,
		`state.localAgent.url.replace(/^http:/, "ws:").replace(/^https:/, "wss:")`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("secure local-agent contract missing %q", want)
		}
	}
	if strings.Contains(html, `body.local-agent-off .local-action { display: none !important; }`) {
		t.Fatal("desktop local actions must remain visible while the agent is offline")
	}
	if !strings.Contains(html, `class="local-action mobile-local-hidden"`) {
		t.Fatal("local actions must opt into mobile hiding")
	}
}

func TestAppWebLocalAgentCapabilityUI(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`id="localAgentRepairBtn"`,
		`id="localAgentRepairLayer"`,
		`id="copyLocalAgentCommandsBtn"`,
		`id="recheckLocalAgentBtn"`,
		`本机代理未连接。请运行 cm local-agent install && cm local-agent start`,
		`window.matchMedia("(max-width: 640px)")`,
		`mobile-local-hidden`,
		`recordLocalIntent(profile, "vnc")`,
		`localAgentAPI("/open-vnc"`,
		`if (!state.localAgent.online)`,
		`if (shouldReturn && state.view === "terminalView")`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("local capability UI missing %q", want)
		}
	}
	if strings.Contains(html, `state.terminalConnectedProfiles.has(profile)`) {
		t.Fatal("VNC must not require a prior terminal connection")
	}

	cssData, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "connectmac-workbench.css"))
	if err != nil {
		t.Fatalf("read workbench css: %v", err)
	}
	css := string(cssData)
	for _, want := range []string{
		`@media (max-width: 640px)`,
		`.mobile-local-hidden`,
		`.workbench-mobile-actions`,
		`padding-bottom: 84px`,
	} {
		if !strings.Contains(css, want) {
			t.Fatalf("mobile capability CSS missing %q", want)
		}
	}
}

func TestAppWebLocalAgentDoesNotSwitchEndpointDuringActiveWork(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		`function localAgentEndpointLocked()`,
		`state.terminal.socket && state.terminal.socket.readyState !== WebSocket.CLOSED`,
		`Object.values(state.syncJobs).some((job) => syncJobActive(job))`,
		`...(locked ? [] : [state.localAgent.secureURL, state.localAgent.legacyURL]`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("active endpoint lock missing %q", want)
		}
	}
}
