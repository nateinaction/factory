package workflow

import (
	"fmt"
	"strings"
)

// Repo is a checked owner/name repository name. Its field is unexported, so the only way to
// build a non-zero one is ParseRepo.
type Repo struct {
	name string
}

// ParseRepo parses an owner/name string. On failure it returns the zero Repo and an error.
func ParseRepo(s string) (Repo, error) {
	// The name ends up in paths and URLs, so anything but a plain owner/name is refused.
	if !repoRE.MatchString(s) || strings.HasPrefix(s, "..") || strings.Contains(s, "/..") {
		return Repo{}, fmt.Errorf("repo must be owner/name, got %q", s)
	}

	return Repo{name: s}, nil
}

// String returns the owner/name form, or "" for the zero Repo.
func (r Repo) String() string {
	return r.name
}

// Owner returns the repository owner, the only owner whose factory Apps are ours. It is empty for
// the zero Repo, which fails every factory-authorship check closed.
func (r Repo) Owner() string {
	owner, _, _ := strings.Cut(r.name, "/")

	return owner
}

// ParseIssueRef parses the repository and issue number arguments of a command into one IssueRef. On
// failure it returns the zero IssueRef and an error.
func ParseIssueRef(repo, number string) (IssueRef, error) {
	r, err := ParseRepo(repo)
	if err != nil {
		return IssueRef{}, err
	}

	n, err := ParseIssueNumber(number)
	if err != nil {
		return IssueRef{}, err
	}

	return IssueRef{Repo: r, Number: n}, nil
}

// IssueRef names one issue in one repository.
type IssueRef struct {
	Repo   Repo
	Number int
}

// String formats the reference as owner/name#N, for logs.
func (i IssueRef) String() string {
	return fmt.Sprintf("%s#%d", i.Repo, i.Number)
}
