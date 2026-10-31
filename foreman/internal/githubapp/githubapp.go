// Package githubapp mints GitHub App installation tokens, one App per agent role.
//
// A token is fresh, uncached and limited to one repository; a read-only token carries only the reads
// the agent role's App holds, so the model inside a run can read the issue and the pull request but
// never write. Credentials live in one directory per App, named for its agent role:
// <dir>/<role>/app-id and <dir>/<role>/private-key.pem. An App never uses another App's directory.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The ceilings on a listing. A listing that reaches one is a failure, never a partial answer.
const (
	PageSize         = 100
	MaxPages         = 10
	MaxInstallations = 50
)

const (
	// maxBody bounds one API response.
	maxBody = 8 << 20
	// maxErrorBody bounds how much of GitHub's error body is quoted back.
	maxErrorBody = 500
	// jwtBackdate absorbs clock skew between this host and GitHub.
	jwtBackdate = 60 * time.Second
	// jwtLifetime is how long a JWT lives; GitHub allows ten minutes at most.
	jwtLifetime = 540 * time.Second
)

var (
	// roleRE is what an agent role's name looks like, so it cannot climb out of the credential directory.
	roleRE = regexp.MustCompile(`^[a-z]{1,20}$`)
	// repoRE is OWNER/NAME.
	repoRE = regexp.MustCompile(`^([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)$`)
)

// The permission names a read-only token may ask for.
const (
	permContents     = "contents"
	permIssues       = "issues"
	permMetadata     = "metadata"
	permPullRequests = "pull_requests"
)

// readOnlyNames is the reads of each agent role's App that a read-only token may ask for. GitHub
// refuses a token that asks for more than its App holds. Planner has no Pull requests, Curator no
// Contents or Pull requests; a role not listed gets no read-only token.
func readOnlyNames(role string) ([]string, bool) {
	switch role {
	case "planner", "researcher":
		return []string{permContents, permIssues, permMetadata}, true
	case "implementer", "reviewer":
		return []string{permContents, permIssues, permMetadata, permPullRequests}, true
	case "curator":
		return []string{permIssues, permMetadata}, true
	default:
		return nil, false
	}
}

// ReadOnlyPermissions is the permissions a read-only token of the agent role asks for. An unlisted
// role fails closed.
func ReadOnlyPermissions(role string) (map[string]string, error) {
	names, ok := readOnlyNames(role)
	if !ok {
		return nil, fmt.Errorf("no read-only permissions are set for agent role %q", role)
	}

	out := make(map[string]string, len(names))
	for _, name := range names {
		out[name] = "read"
	}

	return out, nil
}

// Client is the edge: where credentials are and which API to ask.
type Client struct {
	// Dir holds one credential directory per agent role.
	Dir string
	// API is the GitHub REST API root.
	API string
	// HTTP makes the requests; its timeout bounds each one.
	HTTP *http.Client
}

// installation is one installation of an App.
type installation struct {
	ID int64 `json:"id"`
}

// Token is a one-hour token for a single repository, never cached: a cache file would outlive the hour.
func (c Client) Token(ctx context.Context, role, repo string, readOnly bool) (string, error) {
	m := repoRE.FindStringSubmatch(repo)
	if m == nil || strings.Contains(repo, "..") {
		return "", fmt.Errorf("--repo takes OWNER/NAME, got %q", repo)
	}

	jwt, err := c.jwt(role)
	if err != nil {
		return "", err
	}

	var inst installation
	if err := c.call(ctx, http.MethodGet, "/repos/"+repo+"/installation", jwt, nil, &inst); err != nil {
		return "", err
	}

	return c.installationToken(ctx, role, inst.ID, []string{m[2]}, readOnly)
}

// Repositories is OWNER/NAME for every repository the App is installed on, across its installations.
// It returns nothing rather than part of the answer when a listing runs past a ceiling.
func (c Client) Repositories(ctx context.Context, role string) ([]string, error) {
	jwt, err := c.jwt(role)
	if err != nil {
		return nil, err
	}

	installations, err := listing(ctx, c, "/app/installations", jwt, func(raw []byte) ([]installation, error) {
		var out []installation
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode installations: %w", err)
		}

		return out, nil
	})
	if err != nil {
		return nil, err
	}

	if len(installations) > MaxInstallations {
		return nil, fmt.Errorf("%d installations are over the %d listed", len(installations), MaxInstallations)
	}

	var names []string

	for _, inst := range installations {
		token, err := c.installationToken(ctx, role, inst.ID, nil, false)
		if err != nil {
			return nil, err
		}

		repos, err := listing(ctx, c, "/installation/repositories", token, decodeRepositories)
		if err != nil {
			return nil, err
		}

		names = append(names, repos...)
	}

	return names, nil
}

// decodeRepositories is the full names on one page of an installation's repositories.
func decodeRepositories(raw []byte) ([]string, error) {
	var answer struct {
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("decode repositories: %w", err)
	}

	out := make([]string, len(answer.Repositories))
	for i, r := range answer.Repositories {
		out[i] = r.FullName
	}

	return out, nil
}

// listing is every item of a paged listing. A short page ends it; a full last allowed page means the
// listing is longer than this reads, so it fails instead of answering with part.
func listing[T any](ctx context.Context, c Client, path, bearer string, decode func([]byte) ([]T, error)) ([]T, error) {
	var items []T

	for page := 1; page <= MaxPages; page++ {
		raw, err := c.do(ctx, http.MethodGet, fmt.Sprintf("%s?per_page=%d&page=%d", path, PageSize, page), bearer, nil)
		if err != nil {
			return nil, err
		}

		batch, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}

		items = append(items, batch...)

		if len(batch) < PageSize {
			return items, nil
		}
	}

	return nil, fmt.Errorf("%s is longer than %d pages of %d", path, MaxPages, PageSize)
}

// installationToken mints a token of one installation, limited to repos and, if readOnly, to reads.
func (c Client) installationToken(
	ctx context.Context, role string, id int64, repos []string, readOnly bool,
) (string, error) {
	body := map[string]any{}
	if repos != nil {
		body["repositories"] = repos
	}

	if readOnly {
		perms, err := ReadOnlyPermissions(role)
		if err != nil {
			return "", err
		}

		body["permissions"] = perms
	}

	jwt, err := c.jwt(role)
	if err != nil {
		return "", err
	}

	var payload any
	if len(body) > 0 {
		payload = body
	}

	var answer struct {
		Token string `json:"token"`
	}

	path := fmt.Sprintf("/app/installations/%d/access_tokens", id)
	if err := c.call(ctx, http.MethodPost, path, jwt, payload, &answer); err != nil {
		return "", err
	}

	if answer.Token == "" {
		return "", errors.New("GitHub answered without a token")
	}

	return answer.Token, nil
}

// jwt is the App's short-lived credential, signed with its private key.
func (c Client) jwt(role string) (string, error) {
	if !roleRE.MatchString(role) {
		return "", fmt.Errorf("--app takes an agent role name, got %q", role)
	}

	// The root keeps every read inside the credential directory.
	root, err := os.OpenRoot(c.Dir)
	if err != nil {
		return "", fmt.Errorf("open credential directory: %w", err)
	}

	defer func() {
		// Only reads went through it; a failed close loses nothing.
		_ = root.Close()
	}()

	appID, err := root.ReadFile(filepath.Join(role, "app-id"))
	if err != nil {
		return "", fmt.Errorf("read app id: %w", err)
	}

	keyPEM, err := root.ReadFile(filepath.Join(role, "private-key.pem"))
	if err != nil {
		return "", fmt.Errorf("read private key: %w", err)
	}

	key, err := parseKey(keyPEM)
	if err != nil {
		return "", err
	}

	return signJWT(key, strings.TrimSpace(string(appID)), time.Now())
}

// parseKey reads an RSA private key in PKCS#8 or PKCS#1 form.
func parseKey(keyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}

	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("private key is not RSA")
		}

		return key, nil
	}

	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	return key, nil
}

// signJWT is an RS256 JWT issued by the App at now.
func signJWT(key *rsa.PrivateKey, appID string, now time.Time) (string, error) {
	head, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", fmt.Errorf("encode jwt header: %w", err)
	}

	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(), "exp": now.Add(jwtLifetime).Unix(), "iss": appID,
	})
	if err != nil {
		return "", fmt.Errorf("encode jwt claims: %w", err)
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(head) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))

	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}

	return signingInput + "." + enc.EncodeToString(sig), nil
}

// call sends a request and decodes the JSON answer into out.
func (c Client) call(ctx context.Context, method, path, bearer string, body any, out any) error {
	var payload []byte

	if body != nil {
		var err error

		payload, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
	}

	raw, err := c.do(ctx, method, path, bearer, payload)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}

	return nil
}

// do sends one request and returns the bounded body of a 2xx answer.
func (c Client) do(ctx context.Context, method, path, bearer string, payload []byte) ([]byte, error) {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.API+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "foreman-token")

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}

	defer func() {
		// The body was read to its bound; a failed close leaves nothing to recover.
		_ = resp.Body.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// GitHub's error body says why and holds no secret.
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), maxErrorBody))
	}

	return raw, nil
}

// truncate is s cut to at most n bytes.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}

	return s
}
