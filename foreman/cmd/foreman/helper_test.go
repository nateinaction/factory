package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = DescribeTable("parsing the token helper's arguments",
	func(verb string, args []string, want helperArgs, wantErr string) {
		got, err := parseHelperArgs(verb, args)
		if wantErr != "" {
			Expect(err).To(MatchError(ContainSubstring(wantErr)))
			return
		}

		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(want))
	},
	Entry("a token request", "token", []string{"--app", "reviewer", "--repo", "o/r"},
		helperArgs{app: "reviewer", repo: "o/r"}, ""),
	Entry("a read-only token request", "token", []string{"--app=reviewer", "--repo=o/r", "--read-only"},
		helperArgs{app: "reviewer", repo: "o/r", readOnly: true}, ""),
	Entry("a listing", "repositories", []string{"--app", "reviewer"}, helperArgs{app: "reviewer"}, ""),
	Entry("a token without a repo", "token", []string{"--app", "reviewer"}, helperArgs{}, "required: --repo"),
	Entry("a repeated value flag", "token", []string{"--app", "a", "--app", "b", "--repo", "o/r"}, helperArgs{},
		"more than once"),
	Entry("a flag a listing does not take", "repositories", []string{"--app", "a", "--read-only"}, helperArgs{},
		"unrecognized arguments: --read-only"),
	Entry("a value on a switch", "token", []string{"--app", "a", "--repo", "o/r", "--read-only=x"}, helperArgs{},
		"ignored explicit argument 'x'"),
	Entry("a flag with no value", "token", []string{"--app", "a", "--repo"}, helperArgs{}, "expected one argument"),
)
