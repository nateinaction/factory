package workflow

import "fmt"

// Repo is a repository name that has passed ValidateRepo. Its field is unexported, so the only way to
// build a non-zero one is ParseRepo.
type Repo struct {
	name string
}

// ParseRepo parses an owner/name string. On failure it returns the zero Repo and an error.
func ParseRepo(s string) (Repo, error) {
	if err := ValidateRepo(s); err != nil {
		return Repo{}, err
	}

	return Repo{name: s}, nil
}

// String returns the owner/name form, or "" for the zero Repo.
func (r Repo) String() string {
	return r.name
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
