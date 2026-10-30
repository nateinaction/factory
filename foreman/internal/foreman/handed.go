package foreman

import (
	"fmt"
	"os"
	"strings"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// handedTokens are the two tokens `start` left for one run of an agent role on one repository. The
// factory half cannot mint: only the dagu user reads the Apps' keys. It holds these two and nothing
// else, and asking for any other token is an error rather than a fallback.
type handedTokens struct {
	role     workflow.Role
	repo     workflow.Repo
	writable string
	readOnly string
	// factory is the Curator's writable token for the factory repository, where it files what it
	// promotes; empty for every other agent role.
	factoryRepo  workflow.Repo
	factoryToken string
}

// factoryTokenPath is the Curator's token file for the factory repository, beside the run's own.
func (d Deps) factoryTokenPath(role workflow.Role) (string, error) {
	path, err := d.tokenPath(role)

	return path + ".factory", err
}

// factoryRepoFor is the factory repository the Curator files in from repo, when it is another one.
func (d Deps) factoryRepoFor(role workflow.Role, repo workflow.Repo) (workflow.Repo, bool) {
	if role != workflow.Curator || d.Settings.FactoryRepo == "" {
		return workflow.Repo{}, false
	}

	factory, err := workflow.ParseRepo(d.Settings.FactoryRepo)

	return factory, err == nil && factory != repo
}

// readHandedTokens reads the tokens `start` left for the run, at the edge.
func (d Deps) readHandedTokens(role workflow.Role, repo workflow.Repo) (*handedTokens, error) {
	writablePath, err := d.tokenPath(role)
	if err != nil {
		return nil, err
	}

	readOnlyPath, err := d.readOnlyTokenPath(role)
	if err != nil {
		return nil, err
	}

	writable, err := os.ReadFile(writablePath) //nolint:gosec // the token directory joined with a vetted name
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}

	readOnly, err := os.ReadFile(readOnlyPath) //nolint:gosec // the token directory joined with a vetted name
	if err != nil {
		return nil, fmt.Errorf("read read-only token file: %w", err)
	}

	h := &handedTokens{
		role: role, repo: repo,
		writable: strings.TrimSpace(string(writable)), readOnly: strings.TrimSpace(string(readOnly)),
	}

	factory, ok := d.factoryRepoFor(role, repo)
	if !ok {
		return h, nil
	}

	factoryPath, err := d.factoryTokenPath(role)
	if err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(factoryPath) //nolint:gosec // the token directory joined with a vetted name
	if err != nil {
		return nil, fmt.Errorf("read factory token file: %w", err)
	}

	h.factoryRepo, h.factoryToken = factory, strings.TrimSpace(string(raw))

	return h, nil
}

// token is the handed token of the kind asked for, for the run's own agent role and repository only.
func (h *handedTokens) token(role workflow.Role, repo workflow.Repo, readOnly bool) (string, error) {
	if role == h.role && repo == h.factoryRepo && !readOnly && h.factoryToken != "" {
		return h.factoryToken, nil
	}

	if role != h.role || repo != h.repo {
		return "", fmt.Errorf("the factory half holds only the %s's tokens for %s, not %s's for %s",
			h.role, h.repo, role, repo)
	}

	if readOnly {
		return h.readOnly, nil
	}

	return h.writable, nil
}
