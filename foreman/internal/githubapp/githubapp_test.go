package githubapp_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/foreman/internal/githubapp"
)

var _ = Describe("read-only permissions", func() {
	It("are reads only, and omit what the agent role's App lacks", func() {
		got, err := githubapp.ReadOnlyPermissions("curator")
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(Equal(map[string]string{"issues": "read", "metadata": "read"}))
	})

	It("fail closed for an agent role with none", func() {
		_, err := githubapp.ReadOnlyPermissions("paged")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("the client against GitHub", func() {
	var (
		calls  []string
		client githubapp.Client
		reply  http.HandlerFunc
	)

	BeforeEach(func() {
		calls = nil
		reply = nil

		dir := GinkgoT().TempDir()
		Expect(os.Mkdir(filepath.Join(dir, "curator"), 0o700)).To(Succeed())
		Expect(os.WriteFile(filepath.Join(dir, "curator", "app-id"), []byte("1\n"), 0o600)).To(Succeed())

		key, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())

		pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		Expect(os.WriteFile(filepath.Join(dir, "curator", "private-key.pem"), pemKey, 0o600)).To(Succeed())

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.URL.Path)
			reply(w, r)
		}))
		DeferCleanup(server.Close)

		client = githubapp.Client{Dir: dir, API: server.URL, HTTP: server.Client()}
	})

	It("cuts an error body on a rune boundary, since a split rune is not valid text", func() {
		reply = func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(strings.Repeat("€", 400))) // 3 bytes each, so byte 500 is mid-rune
		}

		_, err := client.Token(context.Background(), "curator", "o/r", false)

		Expect(err).To(HaveOccurred())
		Expect(utf8.ValidString(err.Error())).To(BeTrue(), err.Error())
	})

	It("stops listing installations at the first page that is over the ceiling", func() {
		reply = func(w http.ResponseWriter, _ *http.Request) {
			items := make([]map[string]int, githubapp.PageSize)
			for i := range items {
				items[i] = map[string]int{"id": i}
			}

			Expect(json.NewEncoder(w).Encode(items)).To(Succeed())
		}

		_, err := client.Repositories(context.Background(), "curator")

		Expect(err).To(HaveOccurred())
		Expect(calls).To(HaveLen(1))
	})
})
