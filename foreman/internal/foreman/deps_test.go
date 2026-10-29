package foreman

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("a Deps built without telemetry", func() {
	It("opens no-op spans and records no-op instruments", func() {
		d := Deps{}

		_, span := d.tracer().Start(context.Background(), "x")
		span.End()
		Expect(span.SpanContext().IsValid()).To(BeFalse())

		counter, err := d.meter().Int64Counter("x")
		Expect(err).NotTo(HaveOccurred())
		counter.Add(context.Background(), 1)
	})
})

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
