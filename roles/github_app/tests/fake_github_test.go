package tests_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The ceilings roles/github_app/files/github-app-token applies to a listing.
const (
	pageSize         = 100
	maxPages         = 10
	maxInstallations = 50
)

// heldReads is what each App holds that a read-only token may ask for. GitHub refuses a token that
// asks for more than its App holds.
var heldReads = map[string][]string{
	"1": {"contents", "issues", "metadata", "pull_requests"},
	"2": {"contents", "issues", "metadata", "pull_requests"},
	"6": {"contents", "issues", "metadata"},
	"7": {"issues", "metadata"},
}

// installed maps an App id to its installations and the repositories each holds. App 1 is on o/r
// (installation 7); App 2 on o/r and o/q (installations 7 and 8); Apps 6 and 7 on o/r. Apps 3 to 5
// stand past a ceiling of the listing the helper reads: App 3's installation listing is full on its
// last allowed page, App 4's one installation has a full repository listing on its last allowed
// page, and App 5 has more installations than the helper lists at all.
func installed() map[string]map[int][]string {
	paged := map[int][]string{}
	for i := range pageSize * maxPages {
		paged[i] = []string{"o/paged"}
	}
	wide := make([]string, pageSize*maxPages)
	for i := range wide {
		wide[i] = fmt.Sprintf("o/wide-%d", i)
	}
	crowded := map[int][]string{}
	for i := range maxInstallations + 1 {
		crowded[i] = []string{"o/crowded"}
	}
	return map[string]map[int][]string{
		"1": {7: {"o/r"}},
		"2": {7: {"o/r"}, 8: {"o/q"}},
		"3": paged,
		"4": {9: wide},
		"5": crowded,
		"6": {7: {"o/r"}},
		"7": {7: {"o/r"}},
	}
}

// fakeGitHub is an in-process GitHub API. Each App is told apart by the iss claim of the JWT it
// presents, and the token it mints names that App, so a spec can tell whose credential minted what.
func fakeGitHub() *httptest.Server {
	apps := installed()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			serveGet(w, r, apps)
		case http.MethodPost:
			servePost(w, r)
		default:
			reply(w, http.StatusInternalServerError, map[string]string{"message": "unexpected " + r.Method})
		}
	}))
}

func reply(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// A failed write means the client went away; there is no one left to tell.
	_ = json.NewEncoder(w).Encode(body)
}

// iss is the issuer claim of the JWT in an Authorization header; empty if there is none.
func iss(auth string) string {
	parts := strings.Split(strings.TrimPrefix(auth, "Bearer "), ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Iss json.RawMessage `json:"iss"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return strings.Trim(string(claims.Iss), `"`)
}

// page is the slice of items one page asks for; a listing that ends asks for a short page.
func page[T any](r *http.Request, items []T) []T {
	size, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || size < 1 {
		size = pageSize
	}
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		n = 1
	}
	start := min((n-1)*size, len(items))
	return items[start:min(start+size, len(items))]
}

func serveGet(w http.ResponseWriter, r *http.Request, apps map[string]map[int][]string) {
	auth := r.Header.Get("Authorization")
	path := r.URL.Path
	switch {
	case path == "/app/installations":
		ids := []int{}
		for id := range apps[iss(auth)] {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		out := []map[string]int{}
		for _, id := range page(r, ids) {
			out = append(out, map[string]int{"id": id})
		}
		reply(w, http.StatusOK, out)
	case strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/installation"):
		repo := strings.TrimSuffix(strings.TrimPrefix(path, "/repos/"), "/installation")
		for id, repos := range apps[iss(auth)] {
			if slices.Contains(repos, repo) {
				reply(w, http.StatusOK, map[string]int{"id": id})
				return
			}
		}
		reply(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	case path == "/installation/repositories":
		serveRepositories(w, r, apps)
	default:
		reply(w, http.StatusInternalServerError, map[string]string{"message": "unexpected " + path})
	}
}

// serveRepositories answers for an installation token, which reads tok-<app>-<installation>-...
func serveRepositories(w http.ResponseWriter, r *http.Request, apps map[string]map[int][]string) {
	fields := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "-")
	if len(fields) < 3 {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad token"})
		return
	}
	inst, err := strconv.Atoi(fields[2])
	if err != nil {
		reply(w, http.StatusUnauthorized, map[string]string{"message": "bad token"})
		return
	}
	out := []map[string]string{}
	for _, repo := range page(r, apps[fields[1]][inst]) {
		out = append(out, map[string]string{"full_name": repo})
	}
	reply(w, http.StatusOK, map[string]any{"repositories": out})
}

func servePost(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 5 || parts[1] != "app" || parts[2] != "installations" || parts[4] != "access_tokens" {
		reply(w, http.StatusInternalServerError, map[string]string{"message": "unexpected " + r.URL.Path})
		return
	}
	var body struct {
		Repositories []string          `json:"repositories"`
		Permissions  map[string]string `json:"permissions"`
	}
	// An empty body asks for a token of everything; anything else must parse.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			reply(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
			return
		}
	}
	app := iss(r.Header.Get("Authorization"))
	scope := "all"
	if body.Repositories != nil {
		scope = strings.Join(body.Repositories, ",")
	}
	suffix := ""
	if body.Permissions != nil {
		names, ok := readOnlyNames(app, body.Permissions)
		if !ok {
			reply(w, http.StatusUnprocessableEntity, map[string]string{"message": "not a read-only token request this App holds"})
			return
		}
		suffix = "-ro-" + strings.Join(names, "+")
	}
	reply(w, http.StatusCreated, map[string]string{"token": fmt.Sprintf("tok-%s-%s-%s%s", app, parts[3], scope, suffix)})
}

// readOnlyNames is the sorted permission names of a request that asks only for reads its App holds;
// a request that is empty, writes, or asks for more is not ok.
func readOnlyNames(app string, permissions map[string]string) ([]string, bool) {
	names := []string{}
	for name, level := range permissions {
		if level != "read" || !slices.Contains(heldReads[app], name) {
			return nil, false
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, len(names) > 0
}
