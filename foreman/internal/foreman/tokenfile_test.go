package foreman

import (
	"os"
	"path/filepath"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("writeTokenFile", func() {
	var path string

	BeforeEach(func() {
		path = filepath.Join(GinkgoT().TempDir(), "gh-token-implementer-run-1")
	})

	It("leaves the token with the restrictive group-readable mode, and nothing else behind", func() {
		Expect(writeTokenFile(path, "tok")).To(Succeed())

		raw, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(raw)).To(Equal("tok"))

		st, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(st.Mode().Perm()).To(Equal(os.FileMode(0o640)))

		entries, err := os.ReadDir(filepath.Dir(path))
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(HaveLen(1), "a temporary file was left beside the token")
	})

	It("never shows a reader an empty or partial token while it is replaced", func() {
		const old, fresh = "old-token-0123456789", "new-token-9876543210"
		Expect(writeTokenFile(path, old)).To(Succeed())

		stop := make(chan struct{})
		bad := make(chan string, 1)

		var wg sync.WaitGroup

		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				raw, err := os.ReadFile(path)
				if err == nil && string(raw) != old && string(raw) != fresh {
					select {
					case bad <- string(raw):
					default:
					}

					return
				}
			}
		})

		for i := range 300 {
			next := fresh
			if i%2 == 1 {
				next = old
			}

			Expect(writeTokenFile(path, next)).To(Succeed())
		}

		close(stop)
		wg.Wait()
		Expect(bad).NotTo(Receive(), "a reader saw a partial token")
	})
})

var _ = Describe("Deps.tokenPath", func() {
	It("refuses a run id that could leave the token directory", func() {
		d := Deps{RunID: "../x"}
		d.Settings.TokenDir = "/run/factory"

		_, err := d.tokenPath("implementer")
		Expect(err).To(HaveOccurred())
	})

	It("names the file for the run", func() {
		d := Deps{RunID: "run-1"}
		d.Settings.TokenDir = "/run/factory"

		Expect(d.tokenPath("implementer")).To(Equal("/run/factory/gh-token-implementer-run-1"))
	})
})
