package foreman

import (
	"bytes"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// errWriter is a destination that refuses every write.
type errWriter struct{ err error }

// Write refuses the write with its error.
func (w errWriter) Write([]byte) (int, error) { return 0, w.err }

var _ = Describe("redaction", func() {
	// secret is at least as long as any tail the writer keeps for a secret cut by its buffer's end.
	const secret = "ghp_ABCDEFGHIJ"

	It("writes out a line longer than its buffer without waiting for a newline", func() {
		var out bytes.Buffer

		w := newRedactWriter(&out, []string{secret})
		line := secret + strings.Repeat("b", maxPending)

		n, err := w.Write([]byte(line))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(Equal(len(line)))
		Expect(out.Len()).To(BeNumerically(">", 0), "a full buffer is written before any newline")
		Expect(out.String()).NotTo(ContainSubstring(secret))

		Expect(w.flush()).To(Succeed())
		Expect(out.String()).NotTo(ContainSubstring(secret))
	})

	It("redacts a secret split across the buffer's flush boundary", func() {
		var out bytes.Buffer

		w := newRedactWriter(&out, []string{secret})

		// The first write fills the buffer and leaves the start of a secret in the tail it holds back.
		_, err := w.Write([]byte(strings.Repeat("a", maxPending+1) + secret[:5]))
		Expect(err).NotTo(HaveOccurred())
		Expect(out.Len()).To(BeNumerically(">", 0))

		_, err = w.Write([]byte(secret[5:] + "\n"))
		Expect(err).NotTo(HaveOccurred())
		Expect(w.flush()).To(Succeed())

		Expect(out.String()).NotTo(ContainSubstring(secret))
		Expect(out.String()).To(ContainSubstring(redacted))
	})

	DescribeTable("removes every secret when secrets overlap",
		func(secrets []string) {
			Expect(redact("abcdef", secrets)).NotTo(ContainSubstring("abc"))
			Expect(redact("abcdef", secrets)).NotTo(ContainSubstring("abcdef"))
		},
		Entry("the shorter secret first", []string{"abc", "abcdef"}),
		Entry("the longer secret first", []string{"abcdef", "abc"}),
	)

	It("skips an empty secret, so an unset GH_TOKEN cannot match everywhere", func() {
		Expect(redact("gh token is fine", []string{"", "fine"})).To(Equal("gh token is " + redacted))
		Expect(redact("unchanged", []string{""})).To(Equal("unchanged"))
	})

	It("returns the destination's write error for a complete line, with every byte consumed", func() {
		sentinel := errors.New("destination is full")
		w := newRedactWriter(errWriter{err: sentinel}, []string{secret})
		p := []byte("a line with " + secret + "\n")

		n, err := w.Write(p)
		Expect(err).To(MatchError(sentinel))
		Expect(n).To(Equal(len(p)), "the writer buffered p, so a caller must not write it again")
	})

	It("returns the destination's write error for a full buffer, with every byte consumed", func() {
		sentinel := errors.New("destination is full")
		w := newRedactWriter(errWriter{err: sentinel}, []string{secret})
		p := []byte(secret + strings.Repeat("b", maxPending))

		n, err := w.Write(p)
		Expect(err).To(MatchError(sentinel))
		Expect(n).To(Equal(len(p)), "the writer buffered p, so a caller must not write it again")
	})

	It("returns the destination's write error when flushing the last line", func() {
		sentinel := errors.New("destination is full")
		w := newRedactWriter(errWriter{err: sentinel}, []string{secret})

		n, err := w.Write([]byte("an unfinished line with " + secret))
		Expect(err).NotTo(HaveOccurred())
		Expect(n).To(BeNumerically(">", 0))

		Expect(w.flush()).To(MatchError(sentinel))
	})
})
