package tests_test

import (
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	skillsDir  = "../files/skills"
	mainTasks  = "../tasks/main.yml"
	groupVars  = "../../../group_vars/factory.yml"
	upstreamAt = "49dd158d1076134a641b33efb035946536778336"
)

// forks maps each forked skill to its upstream path.
var forks = []struct{ name, upstream string }{
	{"grilling", "skills/productivity/grilling"},
	{"domain-modeling", "skills/engineering/domain-modeling"},
	{"wayfinder", "skills/engineering/wayfinder"},
	{"to-spec", "skills/engineering/to-spec"},
	{"to-tickets", "skills/engineering/to-tickets"},
	{"writing-for-agents", "skills/productivity/writing-for-agents"},
	{"research", "skills/engineering/research"},
	{"implement", "skills/engineering/implement"},
	{"tdd", "skills/engineering/tdd"},
	{"code-review", "skills/engineering/code-review"},
	{"diagnosing-bugs", "skills/engineering/diagnosing-bugs"},
	{"pr", "skills/engineering/pr"},
	{"retro", "skills/engineering/retro"},
}

func skill(name string) string {
	GinkgoHelper()
	return readText(filepath.Join(skillsDir, name, "SKILL.md"))
}

// anySkillContains reports whether any file under the skills directory contains the text.
func anySkillContains(text string) bool {
	GinkgoHelper()
	for _, f := range files(skillsDir) {
		if strings.Contains(readText(f), text) {
			return true
		}
	}
	return false
}

var _ = Describe("the factory's own skills", func() {
	for _, f := range forks {
		It(f.name+" fork", func() {
			path := filepath.Join(skillsDir, f.name, "SKILL.md")
			Expect(path).To(BeARegularFile(), f.name+": no SKILL.md")
			text := skill(f.name)
			Expect(text).To(ContainSubstring("\nname: "+f.name+"\n"), f.name+": frontmatter name differs from its directory")
			Expect(text).To(ContainSubstring("Forked from mattpocock/skills "+f.upstream+" at "+upstreamAt),
				f.name+": no provenance header for "+f.upstream+" at "+upstreamAt)
		})
	}

	It("curate skill", func() {
		Expect(skill("curate")).To(ContainSubstring("Factory-only skill"), "curate: no factory-only header")
	})

	It("a fork keeps the upstream setup instruction", func() {
		Expect(anySkillContains("setup-matt-pocock-skills")).To(BeFalse())
	})
	It("a fork asks the user", func() {
		Expect(anySkillContains("ask the user")).To(BeFalse())
	})

	It("skill output fields", func() {
		grilling := skill("grilling")
		Expect(grilling).To(ContainSubstring(`"kind": "questions"`), "grilling: no kind/questions field")
		Expect(grilling).NotTo(ContainSubstring("goes in `questions`"), "grilling: stale questions field")
		implement := skill("implement")
		Expect(implement).To(ContainSubstring("`replies`"), "implement: no replies field")
		Expect(implement).To(ContainSubstring("`status`"), "implement: no reply status field")
	})

	It("code-review verdict", func() {
		review := skill("code-review")
		Expect(review).To(ContainSubstring("Do not dispatch sub-agents"), "code-review: sub-agents are not denied")
		Expect(review).To(ContainSubstring("REQUEST_CHANGES"), "code-review: no REQUEST_CHANGES verdict")
		Expect(review).To(ContainSubstring("APPROVE"), "code-review: no APPROVE verdict")
	})

	It("skills installed, upstream gone", func() {
		tasks := readText(mainTasks)
		Expect(tasks).To(ContainSubstring("src: skills/"), "the skills are not installed")
		Expect(tasks).NotTo(ContainSubstring("ansible.builtin.git"), "Ansible still clones upstream")
		_, err := os.Stat(groupVars)
		Expect(err).NotTo(HaveOccurred())
		Expect(readText(groupVars)).NotTo(ContainSubstring("mattpocock_skills"), "mattpocock_skills survives")
	})
})
