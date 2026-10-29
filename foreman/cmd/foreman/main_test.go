package main

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = DescribeTable("detecting sudo",
	func(sudoUID string, realUID, effectiveUID int, expected bool) {
		Expect(underSudo(sudoUID, realUID, effectiveUID)).To(Equal(expected))
	},
	Entry("a plain run", "", 1000, 1000, false),
	// sudo -u factory sets the real and effective uid to the target, so only SUDO_UID reveals it.
	Entry("sudo -u factory", "1000", 2000, 2000, true),
	Entry("a setuid root binary", "", 1000, 0, true),
)
