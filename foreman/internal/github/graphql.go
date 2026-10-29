package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/nateinaction/factory/foreman/internal/workflow"
)

// The GraphQL documents foreman sends. Minimizing a comment is GraphQL-only: the REST API has no such
// call and cannot report whether the App may make it.
const (
	// canMinimizeQuery asks whether the viewer may minimize one comment.
	canMinimizeQuery = `query($id: ID!) { node(id: $id) { ... on IssueComment { viewerCanMinimize } } }`
	// minimizeMutation minimizes one comment as resolved.
	minimizeMutation = `mutation($id: ID!) { minimizeComment(input: {subjectId: $id, classifier: RESOLVED}) ` +
		`{ minimizedComment { isMinimized } } }`
	// graphQLPath is GitHub's GraphQL endpoint under the API root.
	graphQLPath = "/graphql"
)

// GraphQL sends one query with its variables and decodes the answer's data into out. An answer that
// carries errors is an error: a half-applied mutation is never reported as success.
func (c Client) GraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := c.Call(ctx, http.MethodPost, graphQLPath,
		map[string]any{"query": query, "variables": variables}, &answer); err != nil {
		return err
	}

	if len(answer.Errors) > 0 {
		return fmt.Errorf("github graphql: %s", answer.Errors[0].Message)
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(answer.Data, out); err != nil {
		return fmt.Errorf("github graphql: decode: %w", err)
	}

	return nil
}

// CanMinimizeComment asks, live, whether this App may minimize the comment. A comment the viewer may
// not minimize is deleted instead.
func (c Client) CanMinimizeComment(ctx context.Context, nodeID string) (bool, error) {
	var answer struct {
		Node struct {
			ViewerCanMinimize bool `json:"viewerCanMinimize"`
		} `json:"node"`
	}

	if err := c.GraphQL(ctx, canMinimizeQuery, map[string]any{"id": nodeID}, &answer); err != nil {
		return false, err
	}

	return answer.Node.ViewerCanMinimize, nil
}

// MinimizeComment marks the comment minimized as resolved.
func (c Client) MinimizeComment(ctx context.Context, nodeID string) error {
	var answer struct {
		MinimizeComment struct {
			MinimizedComment struct {
				IsMinimized bool `json:"isMinimized"`
			} `json:"minimizedComment"`
		} `json:"minimizeComment"`
	}

	if err := c.GraphQL(ctx, minimizeMutation, map[string]any{"id": nodeID}, &answer); err != nil {
		return err
	}

	if !answer.MinimizeComment.MinimizedComment.IsMinimized {
		return errors.New("github graphql: minimizing the comment did not stick")
	}

	return nil
}

// DeleteComment deletes an issue comment: the fallback when the App cannot minimize it.
func (c Client) DeleteComment(ctx context.Context, repo workflow.Repo, id int64) error {
	return c.Call(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/issues/comments/%d", repo, id), nil, nil)
}
