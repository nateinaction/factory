package workflow

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Bounds on what a promoted issue shows.
const (
	// MaxPromotedClaims bounds how many different claims a promoted issue's lesson shows.
	MaxPromotedClaims = 50
	// MaxPromotedEvidence bounds how much of one report's evidence a promoted issue shows.
	MaxPromotedEvidence = 2_000
)

// Markers of a promoted issue and of the issue it was published as.
const (
	// PromotedMarkerPrefix starts a promoted issue's body.
	PromotedMarkerPrefix = MarkerPrefix + "promoted "
	// PublishedMarkerPrefix starts the issue a private lesson was published as.
	PublishedMarkerPrefix = MarkerPrefix + "published "
	// promotionMarkerFields is how many key=value fields a promotion marker carries.
	promotionMarkerFields = 1
	// promotionGroupField is the marker field that holds the group.
	promotionGroupField = "group"
)

// PromotionMarker keys a promoted issue to the group it came from, so a repeat files no second issue.
func PromotionMarker(g GroupID) string {
	return fmt.Sprintf("%s%s=%s -->", PromotedMarkerPrefix, promotionGroupField, g)
}

// PublishedMarker keys the issue a private lesson was published as to its group.
func PublishedMarker(g GroupID) string {
	return fmt.Sprintf("%s%s=%s -->", PublishedMarkerPrefix, promotionGroupField, g)
}

// ParsePromoted reads a promoted issue's marker. found is false when the body is not a promoted issue.
func ParsePromoted(body string) (GroupID, bool, error) {
	return parsePromotionMarker(body, PromotedMarkerPrefix)
}

// ParsePublished reads a published issue's marker. found is false when it is not one.
func ParsePublished(body string) (GroupID, bool, error) {
	return parsePromotionMarker(body, PublishedMarkerPrefix)
}

// parsePromotionMarker reads one promotion marker line and the group it names.
func parsePromotionMarker(body, prefix string) (GroupID, bool, error) {
	var group GroupID

	spec := markerSpec{prefix: prefix, fields: promotionMarkerFields, what: "promotion"}

	found, err := parseMarkerFields(body, spec, func(key, value string) error {
		if key != promotionGroupField {
			return fmt.Errorf("bad promotion marker field %q", key)
		}

		parsed, err := ParseGroupID(value)
		if err != nil {
			return err
		}

		group = parsed

		return nil
	})

	return group, found, err
}

// PromotionRepo is the repository a promoted group's issue is filed in. A document group stays in the
// repository it is about; a skill or factory group leaves a public repository for the factory repo, and
// stays where it is in a private one until a writer publishes it. A zero factory repo fails closed.
func PromotionRepo(g GroupID, repo Repo, private bool, factoryRepo Repo) (Repo, error) {
	if g.Target.Kind == TargetDocs || private {
		return repo, nil
	}

	if factoryRepo == (Repo{}) {
		return Repo{}, errors.New(
			"no factory repository is configured, so nothing can be promoted out of a public repository")
	}

	return factoryRepo, nil
}

// RenderPromotion draws the issue a promoted group becomes. The lesson and the evidence stay in
// separate sections, so a private repository's lesson can be published on its own.
func RenderPromotion(g GroupID, reports []DashboardReport, private bool) (title, body string, err error) {
	if len(reports) == 0 {
		return "", "", fmt.Errorf("group %s has no reports to promote", g)
	}

	title = "Retro: " + g.String()

	var b strings.Builder

	b.WriteString(PromotionMarker(g))
	fmt.Fprintf(&b, "\n\n# %s / %s\n\nA retro group promoted from the retro dashboard.\n", g.Target, g.Key)
	b.WriteString("\n## Lesson\n\n")

	claims := distinctClaimsOf(reports)
	if len(claims) == 0 {
		b.WriteString("_No claim was recorded._\n")
	}

	for _, claim := range claims {
		b.WriteString("\n- " + claim)
	}

	b.WriteString("\n")

	if private {
		b.WriteString("\n## Private evidence\n\nThis repository is private: only this lesson is copied out by `publish`.\n")
	} else {
		b.WriteString("\n## Evidence\n")
	}

	for _, r := range reports {
		fmt.Fprintf(&b, "\n- %s run `%s` on #%d (%s): %s",
			r.Role, r.Key, r.Issue, r.Severity, Truncate(r.Claim, dashboardClaimChars))

		if evidence := strings.TrimSpace(r.Evidence); evidence != "" {
			fmt.Fprintf(&b, "\n\n  > %s", strings.ReplaceAll(Truncate(evidence, MaxPromotedEvidence), "\n", "\n  > "))
		}
	}

	b.WriteString("\n")

	if n := utf8.RuneCountInString(b.String()); n > CommentLimitChars {
		return "", "", fmt.Errorf("promoted issue is %d characters, over the %d limit", n, CommentLimitChars)
	}

	return title, b.String(), nil
}

// distinctClaimsOf collects the different claims behind a group, first reported first.
func distinctClaimsOf(reports []DashboardReport) []string {
	claims := make([]string, 0, len(reports))
	seen := make(map[string]bool, len(reports))

	for _, r := range reports {
		claim := strings.TrimSpace(r.Claim)
		if claim == "" || seen[claim] {
			continue
		}

		seen[claim] = true
		claims = append(claims, Truncate(claim, dashboardClaimChars))
	}

	return claims[:min(len(claims), MaxPromotedClaims)]
}

// LessonOf reads the lesson out of a promoted issue's body: the text under its Lesson heading, up to
// the next section. A body with no lesson, or one carrying a marker, has none.
func LessonOf(body string) (string, bool) {
	return lessonSection(body)
}

// lessonSection is the text between the Lesson heading and the next one.
func lessonSection(body string) (string, bool) {
	lines := strings.Split(body, "\n")

	start := slices.Index(lines, "## Lesson")
	if start < 0 {
		return "", false
	}

	end := len(lines)

	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i

			break
		}
	}

	lesson := strings.TrimSpace(strings.Join(lines[start+1:end], "\n"))
	if lesson == "" || strings.Contains(lesson, MarkerPrefix) {
		return "", false
	}

	return lesson, true
}

// RenderPublished draws the issue a private repository's lesson is published as. Only the lesson crosses
// over: no evidence and no private issue links do.
func RenderPublished(g GroupID, lesson, by string) (title, body string, err error) {
	trimmed := strings.TrimSpace(lesson)
	if trimmed == "" || strings.Contains(trimmed, MarkerPrefix) {
		return "", "", fmt.Errorf("group %s has no lesson to publish", g)
	}

	title = "Retro: " + g.String()

	var b strings.Builder

	b.WriteString(PublishedMarker(g))
	fmt.Fprintf(&b, "\n\n# %s / %s\n\nA lesson promoted from a private repository's retro dashboard by @%s.\n",
		g.Target, g.Key, by)
	fmt.Fprintf(&b, "\n## Lesson\n\n%s\n", trimmed)

	return title, b.String(), nil
}
