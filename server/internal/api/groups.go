package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pushfree/pushfree/internal/store"
)

// Group management (todo 9). These routes are session-authenticated and live
// under /1/groups.json to match the Pushover-compatible surface. A group's
// GroupKey is a 30-char [A-Za-z0-9] identifier -- the same format as a
// user_key -- so it can be used verbatim in the "user" field of
// /1/messages.json; the store resolves whether a key is a user or a group at
// send time (SendRepo.ResolveRecipients).
//
// Field limits (spec): name and memo are each <= 200 chars.
const (
	maxGroupNameRunes = 200
	maxGroupMemoRunes = 200
)

// groupPublic is the JSON shape returned by GET /1/groups.json. Members are
// user_keys (the same identifier used everywhere else) so the caller never
// sees an internal user id.
type groupPublic struct {
	GroupKey string   `json:"group_key"`
	Name     string   `json:"name"`
	Memo     string   `json:"memo"`
	Members  []string `json:"members"`
}

// --- POST /1/groups.json ----------------------------------------------------

type createGroupRequest struct {
	Name    string `json:"name"`
	Memo    string `json:"memo"`
	Members string `json:"members"` // comma-separated user_keys, optional
}

// createGroup creates a delivery group owned by the session user. Initial
// members may be supplied as a comma-separated list of user_keys; each must
// resolve to a real user. Response: 200 {"status":1,"group_key":"<30-char>"}.
func (a *Accounts) createGroup(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with name")
		return
	}
	name := strings.TrimSpace(req.Name)
	memo := req.Memo
	var errs []string
	if name == "" {
		errs = append(errs, "name is required")
	}
	if len([]rune(name)) > maxGroupNameRunes {
		errs = append(errs, "name must be 200 characters or fewer")
	}
	if len([]rune(memo)) > maxGroupMemoRunes {
		errs = append(errs, "memo must be 200 characters or fewer")
	}
	// Resolve initial member keys to user_ids up front so a bad key fails
	// BEFORE the group row is created (no orphan group on a member error).
	memberIDs, memberErrs := a.resolveMemberKeys(r.Context(), req.Members)
	errs = append(errs, memberErrs...)
	if len(errs) > 0 {
		writeErrors(w, http.StatusBadRequest, errs...)
		return
	}

	// group_key is 30-char [A-Za-z0-9], same generator/format as user_key.
	// Collisions on a 62^30 space are astronomically unlikely; retry on a
	// UNIQUE violation rather than surfacing it to the caller.
	var group *store.Group
	for attempt := 0; attempt < 3; attempt++ {
		gk, err := newGroupKey()
		if err != nil {
			a.logger.Error("create group: generate key", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not create group")
			return
		}
		g := &store.Group{
			UserID:    uid,
			GroupKey:  gk,
			Name:      name,
			Memo:      memo,
			CreatedAt: time.Now().UTC(),
		}
		if _, err := a.repos.Groups.Create(r.Context(), g); err != nil {
			if store.IsUniqueViolation(err) {
				continue // retry with a fresh key
			}
			a.logger.Error("create group: insert", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not create group")
			return
		}
		group = g
		break
	}
	if group == nil {
		a.logger.Error("create group: key allocation exhausted after retries")
		writeErrors(w, http.StatusInternalServerError, "could not allocate group key")
		return
	}

	// Add initial members (if any) now that the group has an id.
	if len(memberIDs) > 0 {
		if err := a.repos.Groups.SetMembers(r.Context(), group.ID, memberIDs, nil); err != nil {
			a.logger.Error("create group: set members", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not add members")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1, "group_key": group.GroupKey})
}

// --- GET /1/groups.json -----------------------------------------------------

// listGroups returns every group owned by the session user with its members
// (as user_keys). Response: 200 {"status":1,"groups":[...]}.
func (a *Accounts) listGroups(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	groups, err := a.repos.Groups.ListByOwner(r.Context(), uid)
	if err != nil {
		a.logger.Error("list groups", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not list groups")
		return
	}
	out := make([]groupPublic, 0, len(groups))
	for _, g := range groups {
		members, err := a.repos.Groups.ListMemberKeys(r.Context(), g.ID)
		if err != nil {
			a.logger.Error("list group members", "group_id", g.ID, "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not list groups")
			return
		}
		if members == nil {
			members = []string{}
		}
		out = append(out, groupPublic{
			GroupKey: g.GroupKey,
			Name:     g.Name,
			Memo:     g.Memo,
			Members:  members,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1, "groups": out})
}

// --- PUT /1/groups.json -----------------------------------------------------

type updateGroupRequest struct {
	GroupKey string `json:"group_key"`
	Name     string `json:"name"`
	Memo     string `json:"memo"`
	Add      string `json:"add"`    // comma-separated user_keys to add
	Remove   string `json:"remove"` // comma-separated user_keys to remove
}

// updateGroup updates a group's name/memo and/or adds/removes members. The
// group is identified by group_key and must belong to the session user; a
// foreign or absent group returns 404 (no cross-user enumeration). Response:
// 200 {"status":1}.
func (a *Accounts) updateGroup(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	var req updateGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with group_key")
		return
	}
	if req.GroupKey == "" {
		writeErrors(w, http.StatusBadRequest, "group_key is required")
		return
	}
	g, err := a.repos.Groups.GetByKey(r.Context(), req.GroupKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "group not found")
			return
		}
		a.logger.Error("update group: lookup", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not update group")
		return
	}
	// Ownership check: a group that exists but belongs to another user is
	// reported as 404 (no enumeration).
	if g.UserID != uid {
		writeErrors(w, http.StatusNotFound, "group not found")
		return
	}

	var errs []string
	name := strings.TrimSpace(req.Name)
	memo := req.Memo
	if req.Name != "" && len([]rune(name)) > maxGroupNameRunes {
		errs = append(errs, "name must be 200 characters or fewer")
	}
	if len([]rune(memo)) > maxGroupMemoRunes {
		errs = append(errs, "memo must be 200 characters or fewer")
	}
	addIDs, addErrs := a.resolveMemberKeys(r.Context(), req.Add)
	errs = append(errs, addErrs...)
	removeIDs, removeErrs := a.resolveMemberKeys(r.Context(), req.Remove)
	errs = append(errs, removeErrs...)
	if len(errs) > 0 {
		writeErrors(w, http.StatusBadRequest, errs...)
		return
	}

	// Update name/memo only when a name was supplied; an empty name on an
	// update means "leave unchanged" (clearing the name is not useful). Memo
	// is always rewritten (it defaults to "" which is a valid clear).
	if req.Name != "" {
		if err := a.repos.Groups.Update(r.Context(), g.ID, name, memo); err != nil {
			a.logger.Error("update group: update", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not update group")
			return
		}
	}
	if len(addIDs) > 0 || len(removeIDs) > 0 {
		if err := a.repos.Groups.SetMembers(r.Context(), g.ID, addIDs, removeIDs); err != nil {
			a.logger.Error("update group: set members", "err", err)
			writeErrors(w, http.StatusInternalServerError, "could not update members")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1})
}

// --- DELETE /1/groups.json --------------------------------------------------

type deleteGroupRequest struct {
	GroupKey string `json:"group_key"`
}

// deleteGroup removes a group and its members. The group must belong to the
// session user; a foreign or absent group returns 404. Response: 200
// {"status":1}.
func (a *Accounts) deleteGroup(w http.ResponseWriter, r *http.Request) {
	uid, _ := getUserID(r.Context())
	var req deleteGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErrors(w, http.StatusBadRequest, "request body must be JSON with group_key")
		return
	}
	if req.GroupKey == "" {
		writeErrors(w, http.StatusBadRequest, "group_key is required")
		return
	}
	g, err := a.repos.Groups.GetByKey(r.Context(), req.GroupKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "group not found")
			return
		}
		a.logger.Error("delete group: lookup", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not delete group")
		return
	}
	if g.UserID != uid {
		writeErrors(w, http.StatusNotFound, "group not found")
		return
	}
	if err := a.repos.Groups.Delete(r.Context(), g.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErrors(w, http.StatusNotFound, "group not found")
			return
		}
		a.logger.Error("delete group", "err", err)
		writeErrors(w, http.StatusInternalServerError, "could not delete group")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 1})
}

// --- helpers ----------------------------------------------------------------

// resolveMemberKeys parses a comma-separated list of user_keys and resolves
// each to a user_id. A key that does not resolve to a real user produces an
// error string (collected, not returned as a Go error, so the caller can
// batch it with other validation errors). Empty/whitespace entries are
// skipped, so "" yields an empty slice and no errors.
func (a *Accounts) resolveMemberKeys(ctx context.Context, csv string) ([]int64, []string) {
	var ids []int64
	var errs []string
	for _, k := range strings.Split(csv, ",") {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if !userKeyRe.MatchString(k) {
			errs = append(errs, "member key is invalid")
			continue
		}
		u, err := a.repos.Users.GetByUserKey(ctx, k)
		if err != nil {
			errs = append(errs, "member key is invalid")
			continue
		}
		ids = append(ids, u.ID)
	}
	return ids, errs
}

// newGroupKey returns a 30-char [A-Za-z0-9] group key, format-identical to a
// user_key. Aliases newUserKey so the send path cannot distinguish the two.
func newGroupKey() (string, error) { return newUserKey() }
