package main

import (
	"bytes"
	"encoding/json"

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

var _ = Describe("the diagnostic logger", func() {
	It("writes one JSON object per line with level, message and attributes", func() {
		GinkgoT().Setenv(logLevelEnv, "info")

		var out bytes.Buffer

		newLogger(&out).Error("boom", "issue", 7)

		var record map[string]any
		Expect(json.Unmarshal(out.Bytes(), &record)).To(Succeed())
		Expect(bytes.Count(out.Bytes(), []byte("\n"))).To(Equal(1))
		Expect(record).To(HaveKeyWithValue("level", "ERROR"))
		Expect(record).To(HaveKeyWithValue("msg", "boom"))
		Expect(record).To(HaveKeyWithValue("issue", BeNumerically("==", 7)))
	})

	DescribeTable("leveling by FACTORY_LOG_LEVEL",
		func(name string, debugShown, infoShown, warnShown bool) {
			GinkgoT().Setenv(logLevelEnv, name)

			var out bytes.Buffer

			logger := newLogger(&out)
			shown := func(log func(string, ...any)) bool {
				out.Reset()
				log("x")

				return out.Len() > 0
			}
			Expect(shown(logger.Debug)).To(Equal(debugShown))
			Expect(shown(logger.Info)).To(Equal(infoShown))
			Expect(shown(logger.Warn)).To(Equal(warnShown))
		},
		Entry("debug", "debug", true, true, true),
		Entry("warn", "warn", false, false, true),
		Entry("unset", "", false, true, true),
		Entry("unknown", "loud", false, true, true),
	)
})
