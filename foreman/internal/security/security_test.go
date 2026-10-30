package security

import (
	"bytes"
	"encoding/json"
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Emit", func() {
	It("writes one event record carrying the run it belongs to, its name, its outcome and its host", func() {
		var logged bytes.Buffer

		log := Scope(slog.New(slog.NewJSONHandler(&logged, nil)),
			Run{Role: "implementer", Repository: "o/r", Issue: 7})

		Emit(log, EventRunCapped, RunStepCap, "blocked.example")

		var record map[string]any
		Expect(json.Unmarshal(logged.Bytes(), &record)).To(Succeed())
		Expect(record).To(HaveKeyWithValue("msg", Message))
		Expect(record).To(HaveKeyWithValue("event", "run_capped"))
		Expect(record).To(HaveKeyWithValue("outcome", "step_cap"))
		Expect(record).To(HaveKeyWithValue("host", "blocked.example"))
		Expect(record).To(HaveKeyWithValue("agent_role", "implementer"))
		Expect(record).To(HaveKeyWithValue("repository", "o/r"))
		Expect(record).To(HaveKeyWithValue("issue", float64(7)))
	})
})
