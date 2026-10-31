package tests_test

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

// app is one App the render playbook installs, with the id and key it should hold.
type app struct{ name, id, key string }

var apps = []app{
	{"implementer", "111", "implementer-key"},
	{"reviewer", "222", "reviewer-key"},
}

// vars are the extra vars that point the role at the current user and a directory under dir.
func vars(dir string, current *user.User, group *user.Group) map[string]any {
	return map[string]any{
		"dagu_user":        current.Username,
		"github_app_group": group.Name,
		"github_app_dir":   filepath.Join(dir, "etc", "github-app"),
		"test_key_dir":     filepath.Join(dir, "keys"),
	}
}

func writeKeys(dir string) {
	GinkgoHelper()
	keys := filepath.Join(dir, "keys")
	Expect(os.MkdirAll(keys, 0o700)).To(Succeed())
	for _, a := range apps {
		Expect(os.WriteFile(filepath.Join(keys, a.name+".pem"), []byte(a.key+"\n"), 0o600)).To(Succeed())
	}
}

// stage returns a scratch directory holding the key files, and the extra vars for rendering into it.
func stage() (string, map[string]any, *user.User) {
	GinkgoHelper()
	current, err := user.Current()
	Expect(err).NotTo(HaveOccurred())
	group, err := user.LookupGroupId(current.Gid)
	Expect(err).NotTo(HaveOccurred())
	dir := GinkgoT().TempDir()
	writeKeys(dir)
	return dir, vars(dir, current, group), current
}

func modeOf(path string) string {
	GinkgoHelper()
	info, err := os.Stat(path)
	Expect(err).NotTo(HaveOccurred())
	return strconv.FormatUint(uint64(info.Mode().Perm()), 8)
}

func ownerOf(path string) string {
	GinkgoHelper()
	info, err := os.Stat(path)
	Expect(err).NotTo(HaveOccurred())
	stat, ok := info.Sys().(*syscall.Stat_t)
	Expect(ok).To(BeTrue(), "no owner on %s", path)
	owner, err := user.LookupId(strconv.FormatUint(uint64(stat.Uid), 10))
	Expect(err).NotTo(HaveOccurred())
	return owner.Username
}

func contents(path string) string {
	GinkgoHelper()
	b, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred())
	return string(b)
}

var _ = Describe("the github_app role's credential install", Ordered, func() {
	var (
		appDir string
		owner  string
	)

	// One render serves every check below.
	BeforeAll(func() {
		_, extra, current := stage()
		roletest.Render("render.yml", roletest.Options{Vars: extra})
		appDir = extra["github_app_dir"].(string)
		owner = current.Username
	})

	It("base directory is 0700", func() {
		Expect(modeOf(appDir)).To(Equal("700"))
	})

	for _, a := range apps {
		Describe(a.name, func() {
			It(a.name+" directory mode", func() {
				Expect(modeOf(filepath.Join(appDir, a.name))).To(Equal("700"))
			})
			It(a.name+" directory owner", func() {
				Expect(ownerOf(filepath.Join(appDir, a.name))).To(Equal(owner))
			})
			for _, f := range []string{"app-id", "private-key.pem"} {
				It(a.name+"/"+f+" is 0600", func() {
					Expect(modeOf(filepath.Join(appDir, a.name, f))).To(Equal("600"))
				})
				It(a.name+"/"+f+" owner", func() {
					Expect(ownerOf(filepath.Join(appDir, a.name, f))).To(Equal(owner))
				})
			}
			It(a.name+"/app-id holds its own id", func() {
				Expect(contents(filepath.Join(appDir, a.name, "app-id"))).To(Equal(a.id + "\n"))
			})
			It(a.name+"/private-key.pem holds its own key", func() {
				Expect(contents(filepath.Join(appDir, a.name, "private-key.pem"))).To(Equal(a.key + "\n"))
			})
			It(a.name+"'s key is not in another App's directory", func() {
				for _, other := range apps {
					if other.name == a.name {
						continue
					}
					Expect(contents(filepath.Join(appDir, other.name, "private-key.pem"))).NotTo(ContainSubstring(a.key))
				}
			})
		})
	}
})

var _ = Describe("the github_app role given an App for no agent role", Ordered, func() {
	var (
		renderErr error
		janitor   string
	)

	BeforeAll(func() {
		_, extra, _ := stage()
		extra["github_apps"] = map[string]any{"janitor": map[string]any{"app_id": "1", "private_key_file": "/dev/null"}}
		janitor = filepath.Join(extra["github_app_dir"].(string), "janitor")
		_, renderErr = roletest.TryRender("render.yml", roletest.Options{Vars: extra})
	})

	It("an App for an unknown agent role fails the play", func() {
		Expect(renderErr).To(HaveOccurred())
	})

	It("nothing installed for the rejected App", func() {
		Expect(janitor).NotTo(BeADirectory())
	})
})
