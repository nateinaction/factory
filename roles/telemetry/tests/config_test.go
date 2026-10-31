package tests_test

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

const adminPassword = "test-admin-password"

// renderVars point Grafana's config at the current user and set its admin password.
func renderVars(password string) map[string]any {
	GinkgoHelper()
	current, err := user.Current()
	Expect(err).NotTo(HaveOccurred())
	group, err := user.LookupGroupId(current.Gid)
	Expect(err).NotTo(HaveOccurred())
	vars := map[string]any{"render_owner": current.Username, "render_group": group.Name}
	if password != "" {
		vars["factory_telemetry_grafana_admin_password"] = password
	}
	return vars
}

func matches(pattern, content string) bool {
	GinkgoHelper()
	re, err := regexp.Compile(pattern)
	Expect(err).NotTo(HaveOccurred())
	return re.MatchString(content)
}

// What the telemetry role renders to: every store bound to loopback and bounded by age or size, the
// datasources linked so a run's trace id leads to its metrics and diagnostics, and the memory the
// services may use fitting the budget.
var _ = Describe("the telemetry role's rendered config", Ordered, func() {
	var (
		dir                      string
		tempo, loki, datasources any
		v                        any
	)

	file := func(name string) string { return readFile(dir, name) }

	BeforeAll(func() {
		dir = roletest.Render("telemetry.yml", roletest.Options{Vars: renderVars(adminPassword)}).Dir
		tempo, loki = yamlDoc(dir, "tempo.yaml"), yamlDoc(dir, "loki.yaml")
		datasources, v = yamlDoc(dir, "datasources.yaml"), jsonDoc(dir, "vars.json")
	})

	Describe("not reachable off the tailnet: every listener is loopback", func() {
		It("nothing binds a wildcard address", func() {
			var files []string
			for _, pattern := range []string{"*.yaml", "grafana.ini", "*.service"} {
				found, err := filepath.Glob(filepath.Join(dir, pattern))
				Expect(err).NotTo(HaveOccurred())
				files = append(files, found...)
			}
			Expect(files).NotTo(BeEmpty())
			for _, f := range files {
				Expect(matches(`0\.0\.0\.0|\[::\]`, readFile(f))).To(BeFalse(), f)
			}
		})

		It("tempo listens on loopback only", func() {
			Expect(at(tempo, "server", "http_listen_address")).To(Equal("127.0.0.1"))
			Expect(at(tempo, "server", "grpc_listen_address")).To(Equal("127.0.0.1"))
			port := int(at(v, "factory_telemetry_tempo_otlp_port").(float64))
			Expect(at(tempo, "distributor", "receivers", "otlp", "protocols", "grpc", "endpoint")).
				To(Equal("127.0.0.1:" + strconv.Itoa(port)))
		})

		It("loki listens on loopback only", func() {
			Expect(at(loki, "server", "http_listen_address")).To(Equal("127.0.0.1"))
			Expect(at(loki, "server", "grpc_listen_address")).To(Equal("127.0.0.1"))
		})

		It("grafana listens on loopback only", func() {
			Expect(hasLine(file("grafana.ini"), "http_addr = 127.0.0.1")).To(BeTrue())
		})

		It("prometheus listens on loopback only", func() {
			Expect(file("prometheus.service")).To(ContainSubstring("--web.listen-address=127.0.0.1:"))
		})

		It("prometheus loads its alert rules from a template", func() {
			Expect(file("prometheus.yaml")).To(ContainSubstring("rule_files:"))
			Expect(file("prometheus.yaml")).To(ContainSubstring("/etc/prometheus/alerts.yaml"))
		})

		// The password is the one the spec passed in, not read back from vars.json: that file is
		// world-readable, so it never carries the password.
		It("grafana's admin login is the one the play is given", func() {
			ini := file("grafana.ini")
			Expect(ini).To(ContainSubstring("admin_user = " + text(at(v, "factory_telemetry_grafana_admin_user"))))
			Expect(ini).To(ContainSubstring("admin_password = " + adminPassword))
		})

		// The nix package's homepath is read-only, and Grafana installs the Loki, Prometheus and Tempo
		// datasource plugins at start; without a writable plugin directory every install fails and the
		// datasources never load.
		It("grafana installs its plugins into its writable state directory", func() {
			Expect(hasLine(file("grafana.ini"), "plugins = /var/lib/grafana/plugins")).To(BeTrue())
		})

		// DynamicUser mounts the private state directory noexec, and the datasource plugins are executables.
		It("grafana may execute the plugins it installs, and only those", func() {
			unit := file("grafana.service")
			Expect(hasLine(unit, "StateDirectory=grafana grafana/plugins")).To(BeTrue())
			Expect(hasLine(unit, "ExecPaths=/var/lib/private/grafana/plugins")).To(BeTrue())
		})

		It("grafana reads its root-only config as a systemd credential", func() {
			unit := file("grafana.service")
			Expect(hasLine(unit, "LoadCredential=grafana.ini:/etc/grafana/grafana.ini")).To(BeTrue())
			Expect(unit).To(ContainSubstring("--config=%d/grafana.ini"))
		})
	})

	Describe("Grafana's admin password has no default, and its config is root-only", func() {
		It("group_vars sets no grafana admin password default", func() {
			Expect(matches(`(?m)^factory_telemetry_grafana_admin_password:`, readFile(groupVars))).To(BeFalse())
		})

		It("grafana.ini is not readable by group or world", func() {
			info, err := os.Stat(filepath.Join(dir, "grafana.ini"))
			Expect(err).NotTo(HaveOccurred())
			Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		})

		It("prometheus accepts OTLP and stores exemplars", func() {
			unit := file("prometheus.service")
			Expect(unit).To(ContainSubstring("--web.enable-otlp-receiver"))
			Expect(unit).To(ContainSubstring("exemplar-storage"))
		})
	})

	Describe("every store has a retention bound", func() {
		It("prometheus retention is bounded by age and size", func() {
			unit := file("prometheus.service")
			Expect(matches(`--storage\.tsdb\.retention\.time=[0-9]+[dh]`, unit)).To(BeTrue())
			Expect(matches(`--storage\.tsdb\.retention\.size=[0-9]+[KMG]B`, unit)).To(BeTrue())
		})

		It("tempo retention is bounded by age", func() {
			Expect(text(at(tempo, "backend_scheduler", "provider", "compaction", "compaction", "block_retention"))).To(HaveSuffix("h"))
			Expect(text(at(tempo, "backend_worker", "compaction", "block_retention"))).To(HaveSuffix("h"))
		})

		It("loki retention is bounded by age and enforced", func() {
			Expect(text(at(loki, "limits_config", "retention_period"))).To(HaveSuffix("h"))
			Expect(at(loki, "compactor", "retention_enabled")).To(BeTrue())
		})
	})

	Describe("a run is found from its trace id", func() {
		datasource := func(kind string) any {
			GinkgoHelper()
			for _, d := range list(at(datasources, "datasources")) {
				if at(d, "type") == kind {
					return d
				}
			}
			Fail("no " + kind + " datasource")
			return nil
		}

		It("tempo links a trace to its logs and metrics", func() {
			j := at(datasource("tempo"), "jsonData")
			Expect(at(j, "tracesToLogsV2", "datasourceUid")).To(Equal("loki"))
			Expect(at(j, "tracesToMetrics", "datasourceUid")).To(Equal("prometheus"))
		})

		It("loki turns a trace id in a diagnostic into a Tempo link", func() {
			f := at(datasource("loki"), "jsonData", "derivedFields", 0)
			Expect(at(f, "datasourceUid")).To(Equal("tempo"))
			Expect(at(f, "matcherType")).To(Equal("label"))
			Expect(at(f, "matcherRegex")).To(Equal("trace_id"))
		})

		It("datasource uids are the ones the links name", func() {
			var uids []any
			for _, d := range list(at(datasources, "datasources")) {
				uids = append(uids, at(d, "uid"))
			}
			Expect(uids).To(ConsistOf("prometheus", "tempo", "loki"))
		})
	})

	Describe("memory budget", func() {
		It("service memory limits fit the budget", func() {
			total := 0.0
			for _, mib := range at(v, "factory_telemetry_memory_max_mib").(map[string]any) {
				total += mib.(float64)
			}
			Expect(total).To(BeNumerically("<=", at(v, "factory_telemetry_memory_budget_mib")))
		})

		for _, svc := range []string{"prometheus", "tempo", "loki", "grafana"} {
			It(svc+" unit is memory-capped and sandboxed", func() {
				unit := file(svc + ".service")
				Expect(matches(`(?m)^MemoryMax=[0-9]+M`, unit)).To(BeTrue())
				Expect(hasLine(unit, "DynamicUser=yes")).To(BeTrue())
				Expect(hasLine(unit, "NoNewPrivileges=true")).To(BeTrue())
			})
		}

		// systemd reads StartLimit* only in [Unit]; in [Service] they are ignored.
		It("every backend unit reads its restart limit from [Unit]", func() {
			for _, svc := range []string{"prometheus", "tempo", "loki", "grafana"} {
				seen := map[string]string{}
				section := ""
				for _, raw := range strings.Split(file(svc+".service"), "\n") {
					line := strings.TrimSpace(raw)
					if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
						section = line
						continue
					}
					key, _, _ := strings.Cut(line, "=")
					if key == "StartLimitIntervalSec" || key == "StartLimitBurst" {
						seen[key] = section
					}
				}
				Expect(seen).To(Equal(map[string]string{"StartLimitIntervalSec": "[Unit]", "StartLimitBurst": "[Unit]"}), svc)
			}
		})
	})
})

var _ = Describe("the telemetry role given no Grafana admin password", Ordered, func() {
	var (
		dir       string
		renderErr error
	)

	BeforeAll(func() {
		dir = filepath.Join(GinkgoT().TempDir(), "no-password")
		Expect(os.Mkdir(dir, 0o700)).To(Succeed())
		vars := renderVars("")
		vars["render_dir"] = dir // this render owns the directory, so the spec can see it stay empty
		_, renderErr = roletest.TryRender("telemetry.yml", roletest.Options{Vars: vars})
	})

	// Without the extra var the play is given no password, and must fail closed before writing anything.
	It("an unset grafana admin password fails the play", func() {
		Expect(renderErr).To(HaveOccurred())
	})

	It("the failure names the password variable", func() {
		Expect(renderErr).To(MatchError(ContainSubstring("factory_telemetry_grafana_admin_password is unset")))
	})

	It("nothing is written without a password", func() {
		entries, err := os.ReadDir(dir)
		Expect(err).NotTo(HaveOccurred())
		Expect(entries).To(BeEmpty())
	})
})
