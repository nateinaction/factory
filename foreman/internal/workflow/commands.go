package workflow

import (
	"fmt"
	"slices"
	"strings"
)

// CommandKind is a command a writer types in a comment.
type CommandKind string

// The commands a writer may type.
const (
	// CommandPromote turns a group into an issue.
	CommandPromote CommandKind = "promote"
	// CommandMerge merges two groups into the first.
	CommandMerge CommandKind = "merge"
	// CommandIgnore hides a group from promotion.
	CommandIgnore CommandKind = "ignore"
	// CommandReject refuses a Curator proposal for good.
	CommandReject CommandKind = "reject"
	// CommandPublish copies a private repository's lesson to the factory repo.
	CommandPublish CommandKind = "publish"
	// CommandCurate starts a Curator run.
	CommandCurate CommandKind = "curate"
)

// mergeGroups is how many groups a merge names.
const mergeGroups = 2

// CommandKinds is the closed set of commands.
func CommandKinds() []CommandKind {
	return []CommandKind{CommandPromote, CommandMerge, CommandIgnore, CommandReject, CommandPublish, CommandCurate}
}

// Command is one command parsed from a comment.
type Command struct {
	// Kind is the command.
	Kind CommandKind
	// Groups are the groups it names: one, or two for a merge.
	Groups []GroupID
	// Proposal is the id a reject names.
	Proposal string
}

// ParseCommand reads a command from a comment's first line. found is false when the line is no
// command, so an ordinary comment is left alone; a line that opens with a command word but does not
// parse is an error, so a writer's typo is reported rather than guessed at.
func ParseCommand(body string) (Command, bool, error) {
	fields := strings.Fields(firstLine(body))
	if len(fields) == 0 {
		return Command{}, false, nil
	}

	kind := CommandKind(strings.ToLower(fields[0]))
	if !slices.Contains(CommandKinds(), kind) {
		return Command{}, false, nil
	}

	args := fields[1:]

	switch kind {
	case CommandPromote, CommandIgnore:
		return commandWithGroups(kind, args, 1)
	case CommandMerge:
		return commandWithGroups(kind, args, mergeGroups)
	case CommandReject:
		return commandRejecting(args)
	case CommandPublish:
		if len(args) != 0 {
			return Command{}, true, fmt.Errorf("publish takes no argument, got %q", strings.Join(args, " "))
		}

		return Command{Kind: kind}, true, nil
	case CommandCurate:
		if len(args) != 0 {
			return Command{}, true, fmt.Errorf("curate takes no argument, got %q", strings.Join(args, " "))
		}

		return Command{Kind: kind}, true, nil
	}

	return Command{}, true, fmt.Errorf("unknown command %q", kind)
}

// firstLine is a body's first non-blank line, trimmed.
func firstLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}

	return ""
}

// commandWithGroups parses a command that names exactly want groups.
func commandWithGroups(kind CommandKind, args []string, want int) (Command, bool, error) {
	if len(args) != want {
		return Command{}, true, fmt.Errorf("%s takes %d %s, got %d", kind, want, plural(want, "group"), len(args))
	}

	groups := make([]GroupID, 0, want)

	for _, arg := range args {
		group, err := ParseGroupID(arg)
		if err != nil {
			return Command{}, true, err
		}

		groups = append(groups, group)
	}

	return Command{Kind: kind, Groups: groups}, true, nil
}

// commandRejecting parses a reject, which names a proposal id.
func commandRejecting(args []string) (Command, bool, error) {
	if len(args) != 1 {
		return Command{}, true, fmt.Errorf("%s takes one proposal id, got %d", CommandReject, len(args))
	}

	if !retroNameRE.MatchString(args[0]) {
		return Command{}, true, fmt.Errorf("proposal id %q is not valid", args[0])
	}

	return Command{Kind: CommandReject, Proposal: args[0]}, true, nil
}

// ParseGroupID reads the "target/key" form the dashboard shows. A document target carries a slash of
// its own, so the key is what follows the last one.
func ParseGroupID(text string) (GroupID, error) {
	i := strings.LastIndex(text, "/")
	if i <= 0 || i == len(text)-1 {
		return GroupID{}, fmt.Errorf("group %q is not target/key", text)
	}

	target, err := ParseRetroTarget(text[:i])
	if err != nil {
		return GroupID{}, err
	}

	key := text[i+1:]
	if !retroNameRE.MatchString(key) {
		return GroupID{}, fmt.Errorf("group key %q is not valid", key)
	}

	return GroupID{Target: target, Key: key}, nil
}
