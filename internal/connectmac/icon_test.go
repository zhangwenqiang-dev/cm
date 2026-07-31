package connectmac

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConnectMacIconAssetAndLocalAgentEndpoint(t *testing.T) {
	asset, err := os.ReadFile(filepath.Join("..", "..", "web", "assets", "connectmac-mark.svg"))
	if err != nil {
		t.Fatalf("read ConnectMac icon asset: %v", err)
	}
	if !strings.Contains(string(asset), "#2563eb") || !strings.Contains(string(asset), "<svg") {
		t.Fatalf("icon asset is missing expected SVG mark")
	}

	app := testApp(nil, nil, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/icon.svg", nil)
	rec := httptest.NewRecorder()
	app.newLocalAgentHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("icon status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/svg+xml") {
		t.Fatalf("icon content type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "ConnectMac") {
		t.Fatalf("icon response does not contain ConnectMac mark")
	}

	page, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read Web page: %v", err)
	}
	html := string(page)
	for _, want := range []string{
		`href="assets/connectmac-mark.svg"`,
		`class="brand-title"`,
		`id="terminalBtn" class="local-action mobile-local-hidden"><span class="action-icon" aria-hidden="true">↗</span>连接</button>`,
		`id="vncBtn" class="local-action mobile-local-hidden"><span class="action-icon" aria-hidden="true">▣</span>VNC</button>`,
		`id="syncBtn" class="local-action mobile-local-hidden"><span class="action-icon" aria-hidden="true">⇅</span>传输</button>`,
		`class=\"action-icon\" aria-hidden=\"true\">→</span><span class=\"desktop-entry-label\">进入工作台</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("Web icon contract missing %q", want)
		}
	}
}
