package foreman

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("GitAuthEnv", func() {
	It("gives git the token through the environment, never argv", func() {
		Expect(GitAuthEnv("tok")).To(Equal(map[string]string{
			"GIT_CONFIG_COUNT":    "1",
			"GIT_CONFIG_KEY_0":    "http.https://github.com/.extraheader",
			"GIT_CONFIG_VALUE_0":  "Authorization: Basic eC1hY2Nlc3MtdG9rZW46dG9r", // base64("x-access-token:tok")
			"GIT_TERMINAL_PROMPT": "0",
		}))
	})
})
