package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestGroups covers the todo 9 delivery-group CRUD surface: create, list,
// update (name/memo + add/remove members), delete, and the field limits
// (memo <= 200 chars).
func TestGroups(t *testing.T) {
	// --- CRUD roundtrip ----------------------------------------------------
	// create -> get(list) -> update(name/memo/members) -> delete. Every step
	// returns status:1 and the list reflects the change.
	t.Run("crud_roundtrip", func(t *testing.T) {
		a, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "groupowner@example.com")

		// Register two users to use as members.
		m1 := register(t, newClient(t), base, "gm1@example.com", "password1")
		m2 := register(t, newClient(t), base, "gm2@example.com", "password1")

		// CREATE with one member.
		gk := createGroup(t, c, base, "Team", "initial memo", m1)

		// GET list: one group, one member.
		status, _, body, raw := doReq(t, c, http.MethodGet, base+"/1/groups.json", nil)
		if status != http.StatusOK {
			t.Fatalf("list status=%d body=%s", status, raw)
		}
		groups, _ := body["groups"].([]any)
		if len(groups) != 1 {
			t.Fatalf("want 1 group, got %d: %s", len(groups), raw)
		}
		g0 := groups[0].(map[string]any)
		if g0["group_key"] != gk || g0["name"] != "Team" || g0["memo"] != "initial memo" {
			t.Fatalf("group round-trip mismatch: %+v", g0)
		}
		if members, _ := g0["members"].([]any); len(members) != 1 || members[0] != m1 {
			t.Fatalf("want members=[%s], got %v", m1, members)
		}

		// UPDATE: rename, change memo, add m2.
		status, _, body, raw = doReq(t, c, http.MethodPut, base+"/1/groups.json", map[string]any{
			"group_key": gk,
			"name":      "Team Renamed",
			"memo":      "updated",
			"add":       m2,
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("update status=%d body=%s", status, raw)
		}
		// Verify name/memo + 2 members now.
		_, _, body, raw = doReq(t, c, http.MethodGet, base+"/1/groups.json", nil)
		g0 = body["groups"].([]any)[0].(map[string]any)
		if g0["name"] != "Team Renamed" || g0["memo"] != "updated" {
			t.Fatalf("update not reflected: %+v", g0)
		}
		if members, _ := g0["members"].([]any); len(members) != 2 {
			t.Fatalf("want 2 members after add, got %d: %s", len(members), raw)
		}

		// REMOVE m1 via PUT.
		_, _, body, raw = doReq(t, c, http.MethodPut, base+"/1/groups.json", map[string]any{
			"group_key": gk,
			"name":      "Team Renamed",
			"remove":    m1,
		})
		if body["status"] != float64(1) {
			t.Fatalf("remove member failed: %s", raw)
		}
		_, _, body, _ = doReq(t, c, http.MethodGet, base+"/1/groups.json", nil)
		g0 = body["groups"].([]any)[0].(map[string]any)
		if members, _ := g0["members"].([]any); len(members) != 1 || members[0] != m2 {
			t.Fatalf("want members=[%s] after remove, got %v", m2, members)
		}

		// DELETE.
		status, _, body, raw = doReq(t, c, http.MethodDelete, base+"/1/groups.json", map[string]any{
			"group_key": gk,
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("delete status=%d body=%s", status, raw)
		}
		// List is now empty.
		_, _, body, _ = doReq(t, c, http.MethodGet, base+"/1/groups.json", nil)
		if groups, _ := body["groups"].([]any); len(groups) != 0 {
			t.Fatalf("want 0 groups after delete, got %d", len(groups))
		}
		_ = a
	})

	// --- memo 201 chars -> 400 ---------------------------------------------
	// The memo field is capped at 200 runes (spec).
	t.Run("memo_201_chars_400", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "memotest@example.com")
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/1/groups.json", map[string]any{
			"name": "G",
			"memo": strings.Repeat("x", 201),
		})
		if status != http.StatusBadRequest {
			t.Fatalf("201-char memo status=%d want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		// 200 chars is the boundary: accepted.
		status, _, body, raw = doReq(t, c, http.MethodPost, base+"/1/groups.json", map[string]any{
			"name": "G",
			"memo": strings.Repeat("x", 200),
		})
		if status != http.StatusOK || body["status"] != float64(1) {
			t.Fatalf("200-char memo should be accepted: status=%d body=%s", status, raw)
		}
	})

	// --- name 201 chars -> 400 ---------------------------------------------
	t.Run("name_201_chars_400", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "nametest@example.com")
		status, _, _, raw := doReq(t, c, http.MethodPost, base+"/1/groups.json", map[string]any{
			"name": strings.Repeat("n", 201),
		})
		if status != http.StatusBadRequest {
			t.Fatalf("201-char name status=%d want 400; body=%s", status, raw)
		}
	})

	// --- missing name -> 400 -----------------------------------------------
	t.Run("missing_name_400", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "noname@example.com")
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/1/groups.json", map[string]any{
			"name": "  ",
		})
		if status != http.StatusBadRequest {
			t.Fatalf("blank name status=%d want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	// --- invalid member key -> 400 -----------------------------------------
	// A member key that is not a real user_key fails before the group is
	// created (no orphan group).
	t.Run("invalid_member_key_400", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "badmember@example.com")
		status, _, body, raw := doReq(t, c, http.MethodPost, base+"/1/groups.json", map[string]any{
			"name":    "G",
			"members": "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", // well-formed but unknown
		})
		if status != http.StatusBadRequest {
			t.Fatalf("invalid member status=%d want 400; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
		// No group was created.
		_, _, body, _ = doReq(t, c, http.MethodGet, base+"/1/groups.json", nil)
		if groups, _ := body["groups"].([]any); len(groups) != 0 {
			t.Fatalf("group created despite invalid member: %v", groups)
		}
	})

	// --- update/delete nonexistent group -> 404 ----------------------------
	t.Run("update_nonexistent_404", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "updnonexist@example.com")
		status, _, body, raw := doReq(t, c, http.MethodPut, base+"/1/groups.json", map[string]any{
			"group_key": "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",
			"name":      "X",
		})
		if status != http.StatusNotFound {
			t.Fatalf("update nonexistent status=%d want 404; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	t.Run("delete_nonexistent_404", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t)
		registerLogin(t, c, base, "delnonexist@example.com")
		status, _, body, raw := doReq(t, c, http.MethodDelete, base+"/1/groups.json", map[string]any{
			"group_key": "ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ",
		})
		if status != http.StatusNotFound {
			t.Fatalf("delete nonexistent status=%d want 404; body=%s", status, raw)
		}
		if body["status"] != float64(0) {
			t.Fatalf("status field=%v want 0: %s", body["status"], raw)
		}
	})

	// --- cross-user group not visible (404, no enumeration) ----------------
	// A group owned by user A is 404 to user B on both update and delete.
	t.Run("cross_user_404", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		cA := newClient(t)
		registerLogin(t, cA, base, "ownerA@example.com")
		gk := createGroup(t, cA, base, "private", "", "")

		cB := newClient(t)
		registerLogin(t, cB, base, "ownerB@example.com")
		// B cannot see A's group in their list.
		_, _, body, _ := doReq(t, cB, http.MethodGet, base+"/1/groups.json", nil)
		if groups, _ := body["groups"].([]any); len(groups) != 0 {
			t.Fatalf("B sees A's groups: %v", groups)
		}
		// B cannot update or delete A's group.
		status, _, _, _ := doReq(t, cB, http.MethodPut, base+"/1/groups.json", map[string]any{
			"group_key": gk, "name": "hijacked",
		})
		if status != http.StatusNotFound {
			t.Fatalf("cross-user update status=%d want 404", status)
		}
		status, _, _, _ = doReq(t, cB, http.MethodDelete, base+"/1/groups.json", map[string]any{
			"group_key": gk,
		})
		if status != http.StatusNotFound {
			t.Fatalf("cross-user delete status=%d want 404", status)
		}
	})

	// --- requires session --------------------------------------------------
	t.Run("requires_session_401", func(t *testing.T) {
		_, base := newAppsTestServer(t)
		c := newClient(t) // no session cookie
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodPut, http.MethodDelete} {
			status, _, _, _ := doReq(t, c, method, base+"/1/groups.json", map[string]any{"name": "x"})
			if status != http.StatusUnauthorized {
				t.Fatalf("%s /1/groups.json without session: status=%d want 401", method, status)
			}
		}
	})
}
