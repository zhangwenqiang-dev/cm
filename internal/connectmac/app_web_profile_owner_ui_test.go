package connectmac

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebProfileOwnerLoadingContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "web", "index.html"))
	if err != nil {
		t.Fatalf("read web index: %v", err)
	}
	html := string(data)

	loadProfiles := webFunctionForContractTest(t, html, "async function loadProfiles()", "async function loadReleaseReminders(")
	for _, want := range []string{
		`const seededOwners = { ...state.profileOwners };`,
		`const owner = profile.owners?.[0];`,
		`seededOwners[profile.name] = owner;`,
		`state.profileOwners = seededOwners;`,
		`await loadProfileOwners();`,
	} {
		if !strings.Contains(loadProfiles, want) {
			t.Fatalf("profile loading does not seed embedded owners before reconciliation: missing %q", want)
		}
	}
	if strings.Contains(loadProfiles, `const seededOwners = {};`) {
		t.Fatal("profile loading must not discard current owners before reconciliation")
	}
	copyCurrent := strings.Index(loadProfiles, `const seededOwners = { ...state.profileOwners };`)
	overlayEmbedded := strings.Index(loadProfiles, `seededOwners[profile.name] = owner;`)
	commitFallback := strings.Index(loadProfiles, `state.profileOwners = seededOwners;`)
	reconcile := strings.Index(loadProfiles, `await loadProfileOwners();`)
	if copyCurrent < 0 || overlayEmbedded < 0 || commitFallback < 0 || reconcile < 0 ||
		copyCurrent > overlayEmbedded || overlayEmbedded > commitFallback || commitFallback > reconcile {
		t.Fatal("profile owner fallback must copy current owners, overlay embedded owners, then reconcile")
	}

	loadOwners := webFunctionForContractTest(t, html, "async function loadProfileOwners()", "function applyProfileOwners()")
	if strings.Contains(loadOwners, "clientConfig") || strings.Contains(loadOwners, "user_api") {
		t.Fatal("profile owner loading must not depend on clientConfig.user_api")
	}
	for _, want := range []string{
		`const body = await api("/api/profile-owners");`,
		`const nextOwners = {};`,
		`nextOwners[item.profile_name] = item.owner;`,
		`state.profileOwners = nextOwners;`,
	} {
		if !strings.Contains(loadOwners, want) {
			t.Fatalf("profile owner reconciliation missing %q", want)
		}
	}
	catchStart := strings.Index(loadOwners, "} catch (err) {")
	if catchStart < 0 {
		t.Fatal("profile owner reconciliation must tolerate endpoint failures")
	}
	if strings.Contains(loadOwners[catchStart:], "state.profileOwners =") {
		t.Fatal("failed profile owner reconciliation must preserve the seeded/current owner map")
	}
}

func webFunctionForContractTest(t *testing.T, html, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("web function start missing %q", startMarker)
	}
	end := strings.Index(html[start:], endMarker)
	if end < 0 {
		t.Fatalf("web function end missing %q", endMarker)
	}
	return html[start : start+end]
}
