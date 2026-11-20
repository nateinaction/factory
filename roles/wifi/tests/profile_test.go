package tests_test

import (
	"os"
	"os/user"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/nateinaction/factory/roletest"
)

const (
	ssid = "test-network"
	psk  = "correct-horse-battery"
)

// fakeNmcli puts an nmcli and a netplan that succeed first on PATH, since the handlers run them and
// the test host has neither.
func fakeNmcli() map[string]string {
	GinkgoHelper()
	bin := GinkgoT().TempDir()
	for _, name := range []string{"nmcli", "netplan"} {
		Expect(os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700)).To(Succeed())
	}
	return map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}
}

func extraVars(dir string, extra map[string]any) map[string]any {
	GinkgoHelper()
	current, err := user.Current()
	Expect(err).NotTo(HaveOccurred())
	group, err := user.LookupGroupId(current.Gid)
	Expect(err).NotTo(HaveOccurred())
	vars := map[string]any{
		"wifi_profile_dir":   dir,
		"wifi_netplan_dir":   filepath.Join(dir, "netplan"),
		"wifi_profile_owner": current.Username,
		"wifi_profile_group": group.Name,
	}
	for k, v := range extra {
		vars[k] = v
	}
	return vars
}

var _ = Describe("the wifi role", func() {
	It("removes a netplan Wi-Fi definition and leaves the Ethernet one", func() {
		dir := GinkgoT().TempDir()
		netplan := filepath.Join(dir, "netplan")
		Expect(os.MkdirAll(netplan, 0o700)).To(Succeed())
		wifi := filepath.Join(netplan, "90-NM-wifi.yaml")
		eth := filepath.Join(netplan, "90-NM-eth.yaml")
		Expect(os.WriteFile(wifi, []byte("network:\n  version: 2\n  wifis:\n    wlan0: {}\n"), 0o600)).To(Succeed())
		Expect(os.WriteFile(eth, []byte("network:\n  version: 2\n  ethernets:\n    eth0: {}\n"), 0o600)).To(Succeed())

		roletest.Render("render.yml", roletest.Options{Vars: extraVars(dir, map[string]any{"wifi_ssid": ssid, "wifi_psk": psk}), Env: fakeNmcli()})

		Expect(wifi).NotTo(BeAnExistingFile())
		Expect(eth).To(BeAnExistingFile())
	})

	It("writes a 0600 profile for the SSID holding the passphrase", func() {
		dir := GinkgoT().TempDir()
		roletest.Render("render.yml", roletest.Options{Vars: extraVars(dir, map[string]any{"wifi_ssid": ssid, "wifi_psk": psk}), Env: fakeNmcli()})

		path := filepath.Join(dir, "factory-wifi.nmconnection")
		info, err := os.Stat(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		body, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(body)).To(ContainSubstring("ssid=" + ssid + "\n"))
		Expect(string(body)).To(ContainSubstring("psk=" + psk + "\n"))
	})

	It("fails closed, writing nothing, when the passphrase is unset", func() {
		dir := GinkgoT().TempDir()
		_, err := roletest.TryRender("render.yml", roletest.Options{Vars: extraVars(dir, nil)})
		Expect(err).To(HaveOccurred())
		Expect(os.ReadDir(dir)).To(BeEmpty())
	})

	It("keeps the passphrase and network name out of the playbook log", func() {
		dir := GinkgoT().TempDir()
		rendered := roletest.Render("render.yml", roletest.Options{Vars: extraVars(dir, map[string]any{"wifi_ssid": ssid, "wifi_psk": psk}), Env: fakeNmcli()})
		Expect(rendered.Log).NotTo(ContainSubstring(psk))
		Expect(rendered.Log).NotTo(ContainSubstring(ssid))
	})
})
