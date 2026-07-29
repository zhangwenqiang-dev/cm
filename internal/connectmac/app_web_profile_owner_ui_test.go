package connectmac

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		`const seededOwners = {};`,
		`const owner = profile.owners?.[0];`,
		`seededOwners[profile.name] = owner;`,
		`state.profileOwners = seededOwners;`,
		`await loadProfileOwners();`,
	} {
		if !strings.Contains(loadProfiles, want) {
			t.Fatalf("profile loading does not seed embedded owners before reconciliation: missing %q", want)
		}
	}
	if strings.Contains(loadProfiles, `...state.profileOwners`) {
		t.Fatal("profile loading must clear stale owners omitted by the authoritative profiles response")
	}
	initializeFresh := strings.Index(loadProfiles, `const seededOwners = {};`)
	overlayEmbedded := strings.Index(loadProfiles, `seededOwners[profile.name] = owner;`)
	commitFallback := strings.Index(loadProfiles, `state.profileOwners = seededOwners;`)
	reconcile := strings.Index(loadProfiles, `await loadProfileOwners();`)
	if initializeFresh < 0 || overlayEmbedded < 0 || commitFallback < 0 || reconcile < 0 ||
		initializeFresh > overlayEmbedded || overlayEmbedded > commitFallback || commitFallback > reconcile {
		t.Fatal("profile owner fallback must start fresh, overlay embedded owners, then reconcile")
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

func TestAppWebProfileOwnersFiltersNonAdminAccess(t *testing.T) {
	newApp := func(t *testing.T) App {
		t.Helper()
		app := newWebAutoReleaseTestApp(t)
		for _, member := range []struct {
			name  string
			email string
		}{
			{name: "Assigned Owner", email: "assigned-owner@example.com"},
			{name: "Private Owner", email: "private-owner@example.com"},
		} {
			if _, err := app.MemberStore.AddMember(member.name, member.email, "operator"); err != nil {
				t.Fatalf("add owner %s: %v", member.email, err)
			}
		}
		for _, profile := range []struct {
			name  string
			owner string
		}{
			{name: "assigned-usw2", owner: "assigned-owner@example.com"},
			{name: "private-usw2", owner: "private-owner@example.com"},
		} {
			if _, err := app.MemberStore.UpsertManagedProfile(Profile{Name: profile.name}); err != nil {
				t.Fatalf("upsert profile %s: %v", profile.name, err)
			}
			if _, err := app.MemberStore.SetProfileOwner(profile.name, profile.owner); err != nil {
				t.Fatalf("set owner for %s: %v", profile.name, err)
			}
		}
		return app
	}

	t.Run("admin receives all owners", func(t *testing.T) {
		app := newApp(t)
		req := httptest.NewRequest(http.MethodGet, "/api/profile-owners", nil)
		addWebAuth(t, &app, req, "admin")
		rec := httptest.NewRecorder()
		app.newWebHandler("").ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		owners := decodeProfileOwnersResponse(t, rec)
		if len(owners) != 2 {
			t.Fatalf("owners = %+v", owners)
		}
	})

	t.Run("viewer receives only assigned owner", func(t *testing.T) {
		app := newApp(t)
		req := httptest.NewRequest(http.MethodGet, "/api/profile-owners", nil)
		addWebAuth(t, &app, req, "viewer")
		if _, err := app.MemberStore.AssignProfileAccess("assigned-usw2", "admin@example.com"); err != nil {
			t.Fatalf("assign profile access: %v", err)
		}
		rec := httptest.NewRecorder()
		app.newWebHandler("").ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		owners := decodeProfileOwnersResponse(t, rec)
		if len(owners) != 1 || owners[0].ProfileName != "assigned-usw2" || owners[0].Owner.Email != "assigned-owner@example.com" {
			t.Fatalf("owners = %+v", owners)
		}
		for _, privateValue := range []string{"private-usw2", "private-owner@example.com", "Private Owner"} {
			if strings.Contains(rec.Body.String(), privateValue) {
				t.Fatalf("unassigned owner metadata %q leaked in %s", privateValue, rec.Body.String())
			}
		}
	})
}

func decodeProfileOwnersResponse(t *testing.T, rec *httptest.ResponseRecorder) []PublicProfileOwner {
	t.Helper()
	var response struct {
		Data struct {
			Owners []PublicProfileOwner `json:"owners"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.Data.Owners
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
